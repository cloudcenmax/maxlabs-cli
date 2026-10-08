// Package store persists sessions to disk.
//
// KV Cache effect: **authoritative.** Durability and cache identity are the same
// invariant: a request can only be proven to have been sent if the events that
// produced it still exist. A log that lives only in memory is a prefix nobody
// can reconstruct, and therefore a cache claim nobody can verify.
//
// The CLI deliberately does not use this package. It runs one session and exits,
// and giving it a disk format would mean a format to migrate for a user who
// never asked for history. The app uses it; the command line stays ephemeral.
//
// Layout, under a root such as ~/.censi:
//
//	projects/<id>/project.json        the workspace this belongs to
//	projects/<id>/sessions/<id>.jsonl one committed event per line
//
// The project id is a hash of the absolute workspace path rather than the path
// itself. A path contains separators, can exceed any filename limit, and is not
// unique once a directory is moved or renamed - none of which a hash cares
// about.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"censi/harness/internal/session"
)

// eventsFileSuffix is the extension for a session's event log.
const eventsFileSuffix = ".jsonl"

// Store is a directory of projects.
type Store struct {
	root string
}

// DefaultRoot is where history lives when the caller does not choose.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locating the home directory: %w", err)
	}

	return filepath.Join(home, ".censi"), nil
}

// Open prepares a store at root, creating it if necessary.
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("store: a root directory is required")
	}

	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", root, err)
	}

	return &Store{root: root}, nil
}

// Project is one workspace's history.
type Project struct {
	dir string

	// Workspace is the absolute path this project belongs to.
	Workspace string

	// ID is the stable directory name derived from Workspace.
	ID string
}

// projectFile is the metadata written beside a project's sessions.
type projectFile struct {
	Workspace string `json:"workspace"`
	Created   int64  `json:"created"`
}

// Project returns the project for a workspace, creating it on first use.
func (s *Store) Project(workspace string) (*Project, error) {
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("store: resolving %s: %w", workspace, err)
	}

	// Resolve symlinks so the same directory reached two ways is one project.
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}

	sum := sha256.Sum256([]byte(absolute))
	id := hex.EncodeToString(sum[:8])

	dir := filepath.Join(s.root, "projects", id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return nil, fmt.Errorf("store: creating project %s: %w", id, err)
	}

	project := &Project{dir: dir, Workspace: absolute, ID: id}

	// The metadata file records the readable path. Without it a project
	// directory is an opaque hash and a user cannot tell what it holds.
	meta := filepath.Join(dir, "project.json")
	if _, err := os.Stat(meta); os.IsNotExist(err) {
		if err := writeJSONFile(meta, projectFile{Workspace: absolute, Created: time.Now().Unix()}); err != nil {
			return nil, err
		}
	}

	return project, nil
}

// Projects lists every project in the store, newest first.
func (s *Store) Projects() ([]*Project, error) {
	base := filepath.Join(s.root, "projects")

	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("store: reading %s: %w", base, err)
	}

	var out []*Project

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(base, entry.Name())

		var meta projectFile
		if err := readJSONFile(filepath.Join(dir, "project.json"), &meta); err != nil {
			// A project without readable metadata is skipped rather than fatal:
			// one damaged directory must not hide every other conversation.
			continue
		}

		out = append(out, &Project{dir: dir, Workspace: meta.Workspace, ID: entry.Name()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Workspace < out[j].Workspace })

	return out, nil
}

// Info summarises one recorded session.
type Info struct {
	ID      string
	Updated time.Time
	Events  int

	// Preview is the first thing the user asked, for a history list.
	Preview string
}

// Append adds events to a session's log.
//
// Each event is written as one line and the file is flushed before returning.
// A JSON Lines file is self-healing under truncation: a partial final line can
// be discarded on read, whereas a single JSON document that is cut off is
// unrecoverable.
func (p *Project) Append(sessionID string, events ...session.Event) error {
	if len(events) == 0 {
		return nil
	}

	path := p.sessionPath(sessionID)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	writer := bufio.NewWriter(file)

	for _, event := range events {
		record, err := encodeEvent(event)
		if err != nil {
			return err
		}

		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("store: encoding event %d: %w", event.Seq, err)
		}

		if _, err := writer.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("store: writing event %d: %w", event.Seq, err)
		}
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("store: flushing %s: %w", path, err)
	}

	// Sync so a crash after this call cannot lose an event the caller believes
	// is durable. The cost is one fsync per append, which is the price of the
	// guarantee that makes resume verifiable.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("store: syncing %s: %w", path, err)
	}

	return nil
}

// Load rebuilds a session log from disk.
//
// Events are replayed through the ordinary Append path rather than
// reconstructed directly, so a loaded log is built by exactly the code that
// built the live one. Any divergence between the two would show up here as a
// difference in the derived messages - which is the prefix the provider cached.
func (p *Project) Load(sessionID string) (*session.Log, error) {
	path := p.sessionPath(sessionID)

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	log := session.New(nil)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var record eventRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// A truncated final line is expected after a crash; anything else
			// means the file is not ours. Either way, stopping here keeps what
			// was read rather than discarding a whole conversation.
			break
		}

		payload, err := decodePayload(record.Type, record.Payload)
		if err != nil {
			return nil, err
		}

		if _, err := log.Append(record.Type, payload, record.Surface); err != nil {
			return nil, fmt.Errorf("store: replaying event %d: %w", record.Seq, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", path, err)
	}

	return log, nil
}

// Sessions lists what has been recorded for this project, newest first.
func (p *Project) Sessions() ([]Info, error) {
	dir := filepath.Join(p.dir, "sessions")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("store: reading %s: %w", dir, err)
	}

	var out []Info

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), eventsFileSuffix) || strings.HasSuffix(entry.Name(), reviewFileSuffix) {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), eventsFileSuffix)

		stat, err := entry.Info()
		if err != nil {
			continue
		}

		info := Info{ID: id, Updated: stat.ModTime()}

		if record, err := os.Open(filepath.Join(dir, entry.Name())); err == nil {
			info.Preview, info.Events = summarise(record)

			_ = record.Close()
		}

		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })

	return out, nil
}

// Delete removes a recorded session.
func (p *Project) Delete(sessionID string) error {
	if err := os.Remove(p.sessionPath(sessionID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: removing session %s: %w", sessionID, err)
	}
	if err := os.Remove(p.reviewPath(sessionID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: removing review log for session %s: %w", sessionID, err)
	}

	return nil
}

func (p *Project) sessionPath(sessionID string) string {
	// A session id becomes part of a path, so it is sanitised rather than
	// trusted: one containing a separator would write outside the project.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, sessionID)

	return filepath.Join(p.dir, "sessions", safe+eventsFileSuffix)
}

// summarise counts events and finds the first user message.
func summarise(file *os.File) (string, int) {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	preview := ""
	count := 0

	for scanner.Scan() {
		var record eventRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			break
		}

		count++

		if preview == "" && record.Type == session.EventUserMessage {
			var message session.Message
			if err := json.Unmarshal(record.Payload, &message); err == nil {
				preview = firstText(message)
			}
		}
	}

	return preview, count
}

// firstText returns the first text block of a message, bounded.
func firstText(message session.Message) string {
	for _, block := range message.Content {
		if block.Type == session.BlockText && block.Text != "" {
			text := strings.Join(strings.Fields(block.Text), " ")

			const limit = 120
			if runes := []rune(text); len(runes) > limit {
				return string(runes[:limit]) + "..."
			}

			return text
		}
	}

	return ""
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encoding %s: %w", path, err)
	}

	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("store: writing %s: %w", path, err)
	}

	return nil
}

func readJSONFile(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, into)
}
