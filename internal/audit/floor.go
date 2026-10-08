package audit

import (
	"fmt"
	"path/filepath"
	"strings"
)

// neverAuto are paths whose contents must not be read into a model's context by
// an unattended command.
//
// This is a different concern from mutation. `cat ~/.ssh/id_rsa` changes
// nothing, so a read-only rule would approve it - and the key would then be in
// the transcript, sent to a provider, and cached. The harm is the disclosure,
// not the write.
//
// Matching is on the path as written, so it is a guard against the ordinary
// mistake rather than against an adversary who has already chosen a path. That
// is the right level: the agent is not the attacker, it is the thing that
// occasionally does something unwise.
var neverAutoPaths = []string{
	".ssh/", ".aws/", ".gnupg/", ".kube/", ".docker/config.json",
	".netrc", ".npmrc", ".pypirc", ".git-credentials",
	"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
	"authorized_keys", "known_hosts",
	"credentials.json", "service-account.json",
	".env", ".env.local", ".env.production",
	"login.keychain", "keychain-db",
	"/etc/shadow", "/etc/sudoers",
	".censi/session.json",
}

// neverAutoCommands are commands that are never run unattended, whatever else
// the analysis concludes.
//
// Every safe-command rule in this package is written in the positive: a command
// is safe because it is known to read. An entry here is the opposite - a
// negative rule that survives any future loosening of the positive ones. It is
// the floor a permission mode cannot go below.
var neverAutoCommands = map[string]string{
	"sudo":      "escalates privilege",
	"su":        "escalates privilege",
	"doas":      "escalates privilege",
	"ssh":       "reaches another machine",
	"scp":       "copies to another machine",
	"rsync":     "can copy to another machine",
	"nc":        "opens a network connection",
	"netcat":    "opens a network connection",
	"telnet":    "opens a network connection",
	"curl":      "reaches the network",
	"wget":      "reaches the network",
	"git-push":  "publishes",
	"launchctl": "changes system services",
	"systemctl": "changes system services",
	"defaults":  "changes system configuration",
	"crontab":   "schedules work that outlives this session",
	"at":        "schedules work that outlives this session",
	"shutdown":  "stops the machine",
	"reboot":    "stops the machine",
	"diskutil":  "changes disks",
	"fdisk":     "changes disks",
	"mkfs":      "destroys a filesystem",
	"dd":        "writes raw devices",
	"chown":     "changes ownership",
	"chmod":     "changes permissions",
	"kill":      "signals a process",
	"killall":   "signals processes",
	"pkill":     "signals processes",
	"eval":      "runs a constructed command",
	"exec":      "replaces the running process",
	"source":    "runs a file as a script",
	".":         "runs a file as a script",
}

// onTheFloor reports whether a command may never run unattended.
//
// The name is the point: this is not a rule that can be configured away. A mode
// that auto-approves cannot approve these, which is what makes the mode safe to
// offer at all.
func onTheFloor(command string) (string, bool) {
	if reason, found := touchesSomethingPrivate(command); found {
		return reason, true
	}

	// Only the COMMAND position of each simple command is checked. Scanning
	// every field made `grep -rn x .` match the `.` builtin, so an ordinary
	// recursive search was refused for containing an argument that happens to
	// name a command.
	for _, part := range splitCommands(command) {
		fields := stripWrappers(strings.Fields(part))

		if len(fields) == 0 {
			continue
		}

		// A path-qualified command is judged by its base name, so `/bin/kill`
		// is `kill`.
		name := fields[0]
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}

		if reason, found := neverAutoCommands[name]; found {
			return fmt.Sprintf("%s %s", name, reason), true
		}
	}

	return "", false
}

// touchesSomethingPrivate reports whether a command names a secret path.
func touchesSomethingPrivate(command string) (string, bool) {
	// Normalise so `~/.ssh` and `$HOME/.ssh` are the same test. The check is on
	// the text, because the text is what a person can see and correct.
	normalised := strings.NewReplacer(
		"$HOME/", "", "~/", "", "${HOME}/", "",
	).Replace(command)

	for _, path := range neverAutoPaths {
		needle := strings.TrimPrefix(path, "/")

		if strings.Contains(normalised, needle) {
			return fmt.Sprintf("touches %s, which holds credentials", path), true
		}
	}

	return "", false
}

// stripWrappers removes the prefixes that change how a command runs but not
// what it does.
//
// `timeout 30 ls` is `ls`. Without this the command name is `timeout`, which is
// unknown, so every wrapped read asks a person who then approves it - and a
// prompt that appears for harmless things is one that gets approved without
// reading. The wrapper list is closed: anything not named here is judged as
// itself.
func stripWrappers(fields []string) []string {
	for len(fields) > 0 {
		name := filepath.Base(fields[0])

		switch name {
		case "timeout":
			// timeout takes a duration, and possibly signal flags before it.
			fields = dropFlagsAndOneValue(fields[1:])

		case "nice", "nohup", "stdbuf", "ionice", "command", "builtin", "\\command", "caffeinate":
			fields = dropFlags(fields[1:])

		case "env":
			// env may be followed by assignments, then the command.
			fields = dropAssignments(fields[1:])

		default:
			return fields
		}
	}

	return fields
}

// dropFlags removes leading flags from a wrapped command.
func dropFlags(fields []string) []string {
	for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}

	return fields
}

// dropFlagsAndOneValue removes leading flags and then the wrapper's argument.
func dropFlagsAndOneValue(fields []string) []string {
	fields = dropFlags(fields)

	if len(fields) > 0 {
		fields = fields[1:]
	}

	return fields
}

// dropAssignments removes leading VAR=value pairs.
func dropAssignments(fields []string) []string {
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "=") {
		fields = fields[1:]
	}

	return fields
}
