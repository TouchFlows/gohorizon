package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// LaunchSpec is everything needed to spawn one `queue:work` process. Arguments are
// assembled as an array and executed directly: no shell, no string concatenation
// (handoff §7.1, §13 command-injection requirement).
type LaunchSpec struct {
	PHP        string
	Artisan    string
	Dir        string
	Connection string
	Queue      string
	Name       string

	Sleep   int
	Tries   int
	Timeout int
	Memory  string
	MaxJobs int
	MaxTime int
	Rest    int
	Backoff int

	// Env is appended to the inherited environment.
	Env []string
}

var (
	workerNameRE = regexp.MustCompile(`^gohorizon:[a-z0-9-]+:[a-z0-9-]+:[a-z0-9-]+:[a-z0-9]{8}$`)
	safeArgRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	memoryRE     = regexp.MustCompile(`^([1-9][0-9]*|[1-9][0-9]?%|100%)$`)
)

// WorkerName builds the identity stamped into argv via --name. It is how a restarted
// GoHorizon finds processes it already owns, and how it avoids ever adopting another
// supervisor's workers.
func WorkerName(application, environment, supervisor, uuid string) string {
	return fmt.Sprintf("gohorizon:%s:%s:%s:%s", application, environment, supervisor, shortID(uuid))
}

func workerNamePrefix(application, environment, supervisor string) string {
	return fmt.Sprintf("gohorizon:%s:%s:%s:", application, environment, supervisor)
}

// Args renders the argv passed to the PHP binary.
func (s LaunchSpec) Args() []string {
	args := []string{
		s.Artisan,
		"queue:work",
		s.Connection,
		"--name=" + s.Name,
		"--queue=" + s.Queue,
	}
	args = append(args,
		"--sleep="+strconv.Itoa(s.Sleep),
		"--tries="+strconv.Itoa(s.Tries),
		"--timeout="+strconv.Itoa(s.Timeout),
		"--memory="+s.Memory,
	)
	if s.MaxJobs > 0 {
		args = append(args, "--max-jobs="+strconv.Itoa(s.MaxJobs))
	}
	if s.MaxTime > 0 {
		args = append(args, "--max-time="+strconv.Itoa(s.MaxTime))
	}
	if s.Rest > 0 {
		args = append(args, "--rest="+strconv.Itoa(s.Rest))
	}
	if s.Backoff > 0 {
		args = append(args, "--backoff="+strconv.Itoa(s.Backoff))
	}
	return args
}

// Validate rejects a spec that would produce an unsafe or meaningless command.
func (s LaunchSpec) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if s.PHP == "" || !strings.HasPrefix(s.PHP, "/") || strings.ContainsAny(s.PHP, " \t\n;&|$`") {
		add("php: %q must be an absolute path without shell metacharacters", s.PHP)
	}
	if s.Dir == "" || !strings.HasPrefix(s.Dir, "/") {
		add("dir: %q must be an absolute path", s.Dir)
	}
	if s.Artisan == "" || strings.Contains(s.Artisan, "..") || strings.ContainsAny(s.Artisan, " \t\n;&|$`") {
		add("artisan: %q must be a relative filename inside the application", s.Artisan)
	}
	if !safeArgRE.MatchString(s.Connection) {
		add("connection: %q must be a queue connection name", s.Connection)
	}
	if !safeArgRE.MatchString(s.Queue) {
		add("queue: %q must be a single queue name", s.Queue)
	}
	if !workerNameRE.MatchString(s.Name) {
		add("name: %q must be gohorizon:<application>:<environment>:<supervisor>:<8-char-id>", s.Name)
	}
	if s.Sleep < 0 {
		add("sleep: must be zero or positive")
	}
	if s.Tries < 0 {
		add("tries: must be zero or positive")
	}
	if s.Timeout <= 0 {
		add("timeout: must be positive")
	}
	if !memoryRE.MatchString(s.Memory) {
		add("memory: %q must be megabytes or a percentage", s.Memory)
	}
	if s.MaxJobs < 0 || s.MaxTime < 0 || s.Rest < 0 || s.Backoff < 0 {
		add("max_jobs, max_time, rest, backoff: must be zero or positive")
	}
	// A queue name containing a comma would silently become a multi-queue worker,
	// which is the anti-pattern fixed by ADR 0005.
	if strings.Contains(s.Queue, ",") {
		add("queue: %q must not contain a comma", s.Queue)
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid worker launch spec: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Command builds the process. It uses exec.Command rather than CommandContext so a
// cancelled context never SIGKILLs a worker behind the supervisor's back; stopping is
// always the explicit, logged path in state.go and manager.go.
func (s LaunchSpec) Command() *exec.Cmd {
	cmd := exec.Command(s.PHP, s.Args()...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	setProcessGroup(cmd)
	return cmd
}

// ErrProcessGone means the pid no longer exists (not that probing failed).
var ErrProcessGone = errors.New("process gone")

// ErrProbeUnsupported means this platform has no /proc-style probe.
var ErrProbeUnsupported = errors.New("process probing is not supported on this platform")
