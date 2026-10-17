package greenfield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Artifact struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Summary  string `json:"summary"`
	Revision int    `json:"revision"`
}
type Check struct {
	Log     string `json:"log,omitempty"`
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output"`
}
type Run struct {
	ID        string  `json:"id,omitempty"`
	Archive   string  `json:"archive,omitempty"`
	Status    string  `json:"status"`
	Attempts  int     `json:"attempts"`
	Output    string  `json:"output"`
	Checks    []Check `json:"checks"`
	Bootstrap *Check  `json:"bootstrap,omitempty"`
}
type State struct {
	Files         map[string]string `json:"files"`
	SchemaVersion int               `json:"schema_version"`
	Revision      int               `json:"revision"`
	Artifacts     []Artifact        `json:"artifacts"`
	Approvals     map[string]string `json:"approvals"`
	Runs          map[string]*Run   `json:"runs"`
	References    map[string]string `json:"references"`
}
type Status struct {
	State
	Stage  string              `json:"stage"`
	Gates  map[string]string   `json:"gates"`
	Errors []string            `json:"errors"`
	Ready  []string            `json:"ready"`
	Kinds  map[string][]string `json:"kinds"`
}
type Store struct {
	Root string
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Store{Root: abs}, nil
}
func hash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func SafePath(root, path string) (string, error) {
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, path)
	}
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	ancestor := candidate
	for {
		resolved, e := filepath.EvalSymlinks(ancestor)
		if e == nil {
			r, e := filepath.Rel(root, resolved)
			if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("symlink escapes workspace")
			}
			suffix, _ := filepath.Rel(ancestor, candidate)
			candidate = filepath.Join(resolved, suffix)
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", e
		}
		ancestor = next
	}
	return candidate, nil
}
func (s *Store) Path(path string) (string, error) {
	return SafePath(s.Root, filepath.Join(".maxlabs", "team", path))
}
func emptyState() State {
	return State{SchemaVersion: 1, Artifacts: []Artifact{}, Approvals: map[string]string{}, Runs: map[string]*Run{}, References: map[string]string{}, Files: map[string]string{}}
}
func (s *Store) Load() (State, error) {
	p, err := s.Path("state.json")
	if err != nil {
		return State{}, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return State{}, err
	}
	if state.SchemaVersion != 1 || state.Artifacts == nil || state.Approvals == nil || state.Runs == nil || state.References == nil {
		return State{}, fmt.Errorf("unsupported or invalid team state")
	}
	if state.Files == nil {
		state.Files = map[string]string{}
	}
	return state, nil
}
func (s *Store) Persist(state *State) error {
	p, err := s.Path("state.json")
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(p), ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), p)
}
func (s *Store) Locked(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.Path("")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := s.Path(".lock")
	if err != nil {
		return err
	}
	if err = os.Mkdir(lock, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, e := os.Stat(lock)
		if e != nil {
			return e
		}
		if time.Since(info.ModTime()) < 2*time.Minute {
			return fmt.Errorf("another team operation is active; retry when it finishes")
		}
		recovery := lock + ".recovery"
		if err = os.Mkdir(recovery, 0700); err != nil {
			return fmt.Errorf("another operation is recovering the lease: %w", err)
		}
		recoverErr := func() error {
			defer os.RemoveAll(recovery)
			info, e := os.Stat(lock)
			if e != nil {
				return e
			}
			if time.Since(info.ModTime()) < 2*time.Minute {
				return fmt.Errorf("another operation recovered the lease")
			}
			if e = os.RemoveAll(lock); e != nil {
				return e
			}
			return os.Mkdir(lock, 0700)
		}()
		if recoverErr != nil {
			return recoverErr
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				_ = os.Chtimes(lock, now, now)
			}
		}
	}()
	defer func() { close(stop); <-done; _ = os.RemoveAll(lock) }()
	state, err := s.Load()
	if err != nil {
		return err
	}
	for _, r := range state.Runs {
		if r.Status == "running" || r.Status == "checking" {
			r.Status = "interrupted"
			r.Output = "Previous execution was interrupted; inspect changes before retrying."
		}
	}
	return fn(&state)
}
func (s *Store) Content(a Artifact) (map[string]any, error) {
	if !strings.HasPrefix(a.Path, "artifacts/") || !SafeTaskPath(a.Path) {
		return nil, fmt.Errorf("invalid artifact path")
	}
	p, e := s.Path(a.Path)
	if e != nil {
		return nil, e
	}
	raw, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	if hash(raw) != a.Hash {
		return nil, fmt.Errorf("artifact %s changed outside the workflow; save a new revision", a.ID)
	}
	var d map[string]any
	e = json.Unmarshal(raw, &d)
	return d, e
}

var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)

func SafeTaskPath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\\:*?\"<>|") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return false
		}
		part = strings.ToLower(part)
		if part == "" || part == "." || part == ".." || part == ".maxlabs" || part == ".git" || part == ".env" || (strings.HasPrefix(part, ".env.") && part != ".env.example") {
			return false
		}
	}
	return true
}

var unsafeCheck = regexp.MustCompile("[\\n\\r;&|<>`$(){}\\\\]")
var unsafeConfig = regexp.MustCompile(`(?:^|\s)(?:--env|--configuration|-c|--help|--version|--list-tests|--listTests|--passWithNoTests|--dry-run)(?:=|\s|$)`)

func AllowedCheck(c string) bool {
	if unsafeCheck.MatchString(c) || strings.Contains(c, "..") || unsafeConfig.MatchString(c) {
		return false
	}
	for _, p := range Contract.CheckPrefixes {
		if c == p || strings.HasPrefix(c, p+" ") {
			return true
		}
	}
	return false
}
func list(d map[string]any, k string) []any  { v, _ := d[k].([]any); return v }
func text(d map[string]any, k string) string { v, _ := d[k].(string); return v }
func Strings(d map[string]any, k string) []string {
	out := []string{}
	for _, v := range list(d, k) {
		s, ok := v.(string)
		if ok {
			out = append(out, s)
		}
	}
	return out
}

var arrays = map[string]bool{}

func init() {
	for _, k := range strings.Fields("users workflows scope exclusions open_questions screens components states entities authorization operations acceptance edge_cases depends_on requirements read write checks sources unknowns findings blockers") {
		arrays[k] = true
	}
}
func validate(kind, id string, d map[string]any) error {
	required, ok := Contract.Kinds[kind]
	if !ok || !slug.MatchString(id) {
		return fmt.Errorf("unknown kind or invalid lowercase slug ID")
	}
	if d == nil {
		return fmt.Errorf("content must be a JSON object")
	}
	for _, k := range required {
		v, ok := d[k]
		if !ok {
			return fmt.Errorf("%s.%s is required", kind, k)
		}
		if arrays[k] {
			if _, ok := v.([]any); !ok {
				return fmt.Errorf("%s must be an array", k)
			}
		}
	}
	if strings.TrimSpace(text(d, "summary")) == "" {
		return fmt.Errorf("summary must be nonempty")
	}
	if kind == "task" {
		if value, ok := d["bootstrap"]; ok {
			version, valid := value.(string)
			if !valid || text(d, "role") != "foundation" || !laravelVersion.MatchString(version) {
				return fmt.Errorf("only foundation may bootstrap a confirmed Laravel version")
			}
		}
		role := text(d, "role")
		if role != "foundation" && role != "implementation" && role != "qa" {
			return fmt.Errorf("invalid task role")
		}
		if len(list(d, "requirements")) == 0 || len(list(d, "acceptance")) == 0 || len(list(d, "checks")) == 0 {
			return fmt.Errorf("tasks require requirements, acceptance and checks")
		}
		for _, k := range []string{"read", "write", "depends_on", "requirements", "checks"} {
			for _, v := range list(d, k) {
				p, ok := v.(string)
				if !ok || p == "" {
					return fmt.Errorf("%s must contain strings", k)
				}
				if (k == "read" || k == "write") && !SafeTaskPath(p) {
					return fmt.Errorf("task paths must be exact workspace-relative files outside metadata and credentials")
				}
				if k == "checks" && !AllowedCheck(p) {
					return fmt.Errorf("unsupported verification command: %s", p)
				}
			}
		}
		if role == "qa" && len(list(d, "write")) > 0 {
			return fmt.Errorf("QA tasks are read-only")
		}
	}
	if kind == "design" && strings.TrimSpace(text(d, "prototype_html")) == "" {
		return fmt.Errorf("design requires prototype_html")
	}
	if kind == "integration" {
		switch text(d, "evidence_status") {
		case "documented", "schema_validated", "sandbox_tested", "unknown", "conflicting":
		default:
			return fmt.Errorf("invalid evidence_status")
		}
	}
	return nil
}
func (s *Store) Save(kind, id string, d map[string]any) (Status, error) {
	var out Status
	err := s.Locked(func(state *State) error {
		encoded, _ := json.Marshal(d)
		if len(encoded) > 200000 {
			return fmt.Errorf("artifact exceeds 200 KB; split contracts")
		}
		exists := false
		taskCount := 0
		for _, a := range state.Artifacts {
			if a.Kind == kind && a.ID == id {
				exists = true
			}
			if a.Kind == "task" && a.ID != id {
				taskCount++
			}
		}
		if len(state.Artifacts) >= 128 && !exists {
			return fmt.Errorf("beta supports 128 artifacts per project")
		}
		if kind == "task" && taskCount >= 64 {
			return fmt.Errorf("split release into at most 64 tasks")
		}
		if e := validate(kind, id, d); e != nil {
			return e
		}
		state.Revision++
		path := fmt.Sprintf("artifacts/%s-%s-%d.json", kind, id, state.Revision)
		p, e := s.Path(path)
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		raw, e := json.MarshalIndent(d, "", "  ")
		if e != nil {
			return e
		}
		raw = append(raw, '\n')
		file, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = file.Write(raw)
		ce := file.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		next := []Artifact{}
		for _, a := range state.Artifacts {
			if a.Kind != kind || a.ID != id {
				next = append(next, a)
			}
		}
		state.Artifacts = append(next, Artifact{kind, id, path, hash(raw), text(d, "summary"), state.Revision})
		if kind != "release" && kind != "evidence" {
			for _, r := range state.Runs {
				r.Status = "stale"
			}
		}
		if e = s.Persist(state); e != nil {
			return e
		}
		out, e = s.Status(state)
		return e
	})
	return out, err
}
func (s *Store) Status(state *State) (Status, error) {
	if state == nil {
		loaded, e := s.Load()
		if e != nil {
			return Status{}, e
		}
		state = &loaded
		lock, e := s.Path(".lock")
		if e != nil {
			return Status{}, e
		}
		info, e := os.Stat(lock)
		active := e == nil && time.Since(info.ModTime()) < 2*time.Minute
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return Status{}, e
		}
		if !active {
			for _, run := range state.Runs {
				if run.Status == "running" || run.Status == "checking" {
					run.Status = "interrupted"
					run.Output = "Previous execution was interrupted; inspect changes before retrying."
				}
			}
		}
	}
	out := Status{State: *state, Gates: map[string]string{}, Errors: []string{}, Ready: []string{}, Kinds: Contract.Kinds}
	docs := map[string]map[string]any{}
	byKind := map[string][]Artifact{}
	for _, a := range state.Artifacts {
		byKind[a.Kind] = append(byKind[a.Kind], a)
		d, e := s.Content(a)
		if e != nil {
			out.Errors = append(out.Errors, e.Error())
		} else {
			docs[a.Kind+":"+a.ID] = d
		}
	}
	for gate, kinds := range map[string]string{"design": "brief design", "spec": "brief design spec task integration review", "release": "brief design spec task integration review release"} {
		parts := []string{}
		for _, a := range state.Artifacts {
			if strings.Contains(" "+kinds+" ", " "+a.Kind+" ") {
				parts = append(parts, a.Kind+":"+a.ID+":"+a.Hash)
			}
		}
		sort.Strings(parts)
		refs := []string{}
		for k, v := range state.References {
			refs = append(refs, k+":"+v)
		}
		sort.Strings(refs)
		out.Gates[gate] = hash([]byte(strings.Join(parts, "\n") + "\nreferences:" + strings.Join(refs, "\n")))
	}
	approved := func(g string) bool { return state.Approvals[g] == out.Gates[g] }
	requirements := map[string]bool{}
	covered := map[string]bool{}
	qaCovered := map[string]bool{}
	for _, a := range byKind["spec"] {
		d := docs["spec:"+a.ID]
		if len(list(d, "open_questions")) > 0 {
			out.Errors = append(out.Errors, "Spec has unresolved questions")
		}
		stack, _ := d["stack"].(map[string]any)
		if text(stack, "framework") != "laravel" {
			out.Errors = append(out.Errors, "This beta requires stack.framework: laravel")
		}
		for _, key := range []string{"entities", "authorization", "operations", "acceptance"} {
			if len(list(d, key)) == 0 {
				out.Errors = append(out.Errors, "Spec "+key+" must be defined")
			}
		}
		for _, v := range list(d, "acceptance") {
			ac, ok := v.(map[string]any)
			id := text(ac, "id")
			if !ok || id == "" || strings.TrimSpace(text(ac, "expect")) == "" || requirements[id] {
				out.Errors = append(out.Errors, "Spec acceptance needs unique IDs and concrete expect statements")
			} else {
				requirements[id] = true
			}
		}
	}
	for _, a := range byKind["brief"] {
		if len(list(docs["brief:"+a.ID], "open_questions")) > 0 {
			out.Errors = append(out.Errors, "Brief has unresolved questions")
		}
	}
	for _, a := range byKind["integration"] {
		d := docs["integration:"+a.ID]
		if len(list(d, "unknowns")) > 0 || text(d, "evidence_status") == "unknown" || text(d, "evidence_status") == "conflicting" {
			out.Errors = append(out.Errors, "Integration "+a.ID+" has unresolved evidence")
		}
	}
	for _, a := range byKind["review"] {
		if len(list(docs["review:"+a.ID], "blockers")) > 0 {
			out.Errors = append(out.Errors, "Review has blocking findings")
		}
	}
	if len(byKind["spec"]) != 1 {
		out.Errors = append(out.Errors, "Exactly one spec is required")
	}
	if len(byKind["brief"]) != 1 || len(byKind["design"]) != 1 {
		out.Errors = append(out.Errors, "Exactly one brief and design are required")
	}
	if len(byKind["review"]) == 0 {
		out.Errors = append(out.Errors, "A review artifact is required")
	}
	tasks := map[string]map[string]any{}
	foundations := []string{}
	qa := []string{}
	for _, a := range byKind["task"] {
		d := docs["task:"+a.ID]
		tasks[a.ID] = d
		if text(d, "role") == "foundation" {
			foundations = append(foundations, a.ID)
		}
		if text(d, "role") == "qa" {
			qa = append(qa, a.ID)
		}
	}
	if len(foundations) != 1 || len(qa) == 0 {
		out.Errors = append(out.Errors, "Plan needs exactly one foundation task and at least one independent QA task")
	}
	var ancestors func(string, map[string]bool) map[string]bool
	memo := map[string]map[string]bool{}
	ancestors = func(id string, visiting map[string]bool) map[string]bool {
		found := map[string]bool{}
		if visiting[id] {
			out.Errors = append(out.Errors, "Dependency cycle at "+id)
			return found
		}
		if cached, ok := memo[id]; ok {
			return cached
		}
		next := map[string]bool{}
		for k, v := range visiting {
			next[k] = v
		}
		next[id] = true
		for _, dep := range Strings(tasks[id], "depends_on") {
			if _, ok := tasks[dep]; !ok {
				out.Errors = append(out.Errors, "Missing dependency "+dep)
				continue
			}
			found[dep] = true
			for p := range ancestors(dep, next) {
				found[p] = true
			}
		}
		memo[id] = found
		return found
	}
	passed := func(id string) bool { return state.Runs[id] != nil && state.Runs[id].Status == "passed" }
	allPassed := len(tasks) > 0
	for _, a := range byKind["task"] {
		id := a.ID
		d := tasks[id]
		deps := ancestors(id, map[string]bool{})
		for _, p := range Strings(d, "write") {
			target, err := SafeWorkerPath(s.Root, p)
			if err != nil {
				out.Errors = append(out.Errors, id+": "+err.Error())
				continue
			}
			if info, err := os.Stat(target); err == nil && info.IsDir() {
				out.Errors = append(out.Errors, id+": write path "+p+" is a directory; list exact writable files")
			}
		}
		role := text(d, "role")
		if role == "foundation" {
			version := text(d, "bootstrap")
			if version == "" {
				if _, err := os.Stat(filepath.Join(s.Root, "composer.json")); errors.Is(err, os.ErrNotExist) {
					for _, c := range Strings(d, "checks") {
						if strings.HasPrefix(c, "composer install") {
							out.Errors = append(out.Errors, id+": missing composer.json; set the foundation task bootstrap field to the confirmed Laravel version")
							break
						}
					}
				}
			}
			if version != "" {
				for _, spec := range byKind["spec"] {
					stack, _ := docs["spec:"+spec.ID]["stack"].(map[string]any)
					if major := text(stack, "major"); major != "" && strings.Split(version, ".")[0] != major {
						out.Errors = append(out.Errors, id+": bootstrap version conflicts with spec.stack.major")
					}
				}
			}
		}
		if role != "foundation" && len(foundations) == 1 && !deps[foundations[0]] {
			out.Errors = append(out.Errors, id+" must depend on foundation")
		}
		if role == "foundation" && len(list(d, "depends_on")) > 0 {
			out.Errors = append(out.Errors, "Foundation cannot have dependencies")
		}
		if role == "qa" {
			for other, t := range tasks {
				if text(t, "role") == "implementation" && !deps[other] {
					out.Errors = append(out.Errors, id+" must follow implementation "+other)
				}
			}
		}
		for _, r := range Strings(d, "requirements") {
			if !requirements[r] {
				out.Errors = append(out.Errors, "Unknown requirement "+r)
			} else {
				if role == "qa" {
					qaCovered[r] = true
				} else {
					covered[r] = true
				}
			}
		}
		depsPassed := true
		for _, dep := range Strings(d, "depends_on") {
			if !passed(dep) {
				depsPassed = false
			}
		}
		run := state.Runs[id]
		if approved("design") && approved("spec") && depsPassed && !passed(id) && (run == nil || (run.Status != "running" && run.Attempts < 3)) {
			out.Ready = append(out.Ready, id)
		}
		if !passed(id) {
			allPassed = false
		}
	}
	for r := range requirements {
		if !covered[r] {
			out.Errors = append(out.Errors, "Uncovered requirement "+r)
		}
		if !qaCovered[r] {
			out.Errors = append(out.Errors, "Requirement missing from independent QA: "+r)
		}
	}
	switch {
	case len(byKind["brief"]) == 0:
		out.Stage = "discovery"
	case len(byKind["design"]) == 0:
		out.Stage = "design"
	case !approved("design"):
		out.Stage = "awaiting_design_approval"
	case len(byKind["spec"]) == 0:
		out.Stage = "specification"
	case !approved("spec"):
		out.Stage = "awaiting_spec_approval"
	case len(foundations) == 1 && !passed(foundations[0]):
		out.Stage = "foundation"
	case !allPassed:
		out.Stage = "implementation"
	case len(byKind["release"]) == 0:
		out.Stage = "delivery"
	case approved("release"):
		out.Stage = "complete"
	default:
		out.Stage = "awaiting_release_approval"
	}
	if len(out.Errors) > 0 {
		out.Ready = []string{}
		if approved("spec") {
			out.Stage = "blocked"
		}
	}
	sort.Strings(out.Errors)
	out.Errors = unique(out.Errors)
	return out, nil
}
func unique(in []string) []string {
	out := []string{}
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
func (s *Store) Approve(gate, expected string) (Status, error) {
	var out Status
	err := s.Locked(func(state *State) error {
		status, e := s.Status(state)
		if e != nil {
			return e
		}
		if (gate != "design" && gate != "spec" && gate != "release") || expected != status.Gates[gate] {
			return fmt.Errorf("approval is stale; review the current revision first")
		}
		has := func(kind string) bool {
			for _, a := range state.Artifacts {
				if a.Kind == kind {
					return true
				}
			}
			return false
		}
		if gate == "design" && (!has("brief") || !has("design")) {
			return fmt.Errorf("brief and design are required")
		}
		if gate == "design" {
			for _, a := range state.Artifacts {
				if a.Kind == "brief" {
					d, err := s.Content(a)
					if err != nil {
						return err
					}
					if len(Strings(d, "open_questions")) > 0 {
						return fmt.Errorf("resolve brief open questions before design approval")
					}
				}
			}
		}
		if gate != "design" && (len(status.Errors) > 0 || state.Approvals["design"] != status.Gates["design"]) {
			return fmt.Errorf("resolve readiness errors and approve design first: %s", strings.Join(status.Errors, "; "))
		}
		if gate == "release" {
			if state.Approvals["spec"] != status.Gates["spec"] || !has("release") {
				return fmt.Errorf("release requires approved spec and release artifact")
			}
			for _, a := range state.Artifacts {
				if a.Kind == "task" && (state.Runs[a.ID] == nil || state.Runs[a.ID].Status != "passed") {
					return fmt.Errorf("release requires passing tasks")
				}
			}
		}
		if gate == "spec" {
			state.Files = map[string]string{}
			for _, a := range state.Artifacts {
				if a.Kind == "task" {
					d, e := s.Content(a)
					if e != nil {
						return e
					}
					for _, p := range append(Strings(d, "read"), Strings(d, "write")...) {
						h, e := s.FileHash(p)
						if e != nil {
							return e
						}
						state.Files[p] = h
					}
				}
			}
			if state.Approvals[gate] != expected {
				state.Runs = map[string]*Run{}
			} else {
				for _, run := range state.Runs {
					if run.Status != "passed" {
						run.Attempts = 0
					}
				}
			}
		}
		state.Approvals[gate] = expected
		state.Revision++
		if e = s.Persist(state); e != nil {
			return e
		}
		out, e = s.Status(state)
		return e
	})
	return out, err
}
func (s *Store) Attach(path string) (Status, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Root, path)
	}
	canonical, e := filepath.EvalSymlinks(path)
	if e != nil {
		return Status{}, e
	}
	info, e := os.Stat(canonical)
	if e != nil {
		return Status{}, e
	}
	if !info.IsDir() || canonical == s.Root {
		return Status{}, fmt.Errorf("reference must be another directory")
	}
	var out Status
	e = s.Locked(func(state *State) error {
		state.References[fmt.Sprintf("reference-%d", len(state.References)+1)] = canonical
		state.Revision++
		if e := s.Persist(state); e != nil {
			return e
		}
		var e error
		out, e = s.Status(state)
		return e
	})
	return out, e
}
func (s *Store) Reference(id, path string) (string, error) {
	state, e := s.Load()
	if e != nil {
		return "", e
	}
	root, ok := state.References[id]
	if !ok {
		return "", fmt.Errorf("reference is not attached by the user")
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		part = strings.ToLower(part)
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return "", fmt.Errorf("credentials and Git metadata are not design references")
		}
	}
	p, e := SafePath(root, path)
	if e != nil {
		return "", e
	}
	canonicalRel, _ := filepath.Rel(root, p)
	for _, part := range strings.Split(filepath.ToSlash(canonicalRel), "/") {
		part = strings.ToLower(part)
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return "", fmt.Errorf("credentials and Git metadata are not design references")
		}
	}
	info, e := os.Stat(p)
	if e != nil {
		return "", e
	}
	if info.IsDir() {
		entries, e := os.ReadDir(p)
		if e != nil {
			return "", e
		}
		names := []string{}
		for _, entry := range entries {
			n := entry.Name()
			if entry.Type()&os.ModeSymlink != 0 || n == ".git" || n == "node_modules" || n == "vendor" || strings.HasPrefix(n, ".env") {
				continue
			}
			if entry.IsDir() {
				n += "/"
			}
			names = append(names, n)
			if len(names) >= 500 {
				break
			}
		}
		sort.Strings(names)
		return strings.Join(names, "\n"), nil
	}
	if info.Size() > 200000 {
		return "", fmt.Errorf("reference file exceeds 200 KB")
	}
	raw, e := os.ReadFile(p)
	return "<reference-data>\n" + string(raw) + "\n</reference-data>", e
}

func SafeWorkerPath(root, path string) (string, error) {
	p, e := SafePath(root, path)
	if e != nil {
		return "", e
	}
	rel, e := filepath.Rel(root, p)
	if e != nil || !SafeTaskPath(filepath.ToSlash(rel)) {
		return "", fmt.Errorf("worker path targets protected files")
	}
	metadata, e := SafePath(root, filepath.Join(".maxlabs", "team"))
	if e != nil {
		return "", e
	}
	rel, e = filepath.Rel(metadata, p)
	if e == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return "", fmt.Errorf("worker path targets workflow state")
	}
	return p, nil
}
