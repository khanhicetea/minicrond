package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/khanhicetea/minicrond/internal/model"
)

var cronAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z_0-9]*)\s*=\s*(.*)$`)

type cronEntry struct {
	line int
	text string
	job  model.Definition
}

func crontab(args []string) error {
	fs := flag.NewFlagSet("crontab", flag.ContinueOnError)
	userName := fs.String("user", "", "migrate this user's crontab into a root daemon (requires root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: minicrond crontab [--user NAME]")
	}
	if *userName != "" {
		if os.Geteuid() != 0 {
			return errors.New("--user requires running the CLI as root")
		}
		if err := validateUserName(*userName); err != nil {
			return err
		}
		if *userName == "root" {
			return errors.New("omit --user to migrate root's own crontab")
		}
	}
	if os.Getenv("MINICRON_URL") != "" {
		return errors.New("crontab import requires a local daemon (unset MINICRON_URL)")
	}
	if _, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS); err != nil {
		return errors.New("crontab import requires an interactive terminal for confirmation")
	}
	if *userName != "" {
		var info struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := requestJSON("GET", "/api/v1/daemon", nil, &info); err != nil {
			return err
		}
		if !slices.Contains(info.Capabilities, "run-as") {
			return errors.New("--user requires a root daemon with run-as support")
		}
	}
	original, err := readCrontab(*userName)
	if err != nil {
		return err
	}
	entries, err := parseCrontab(original)
	if err != nil {
		return err
	}
	for i := range entries {
		entries[i].job.RunAs = *userName
	}
	if len(entries) == 0 {
		fmt.Println("no active cron jobs to import")
		return nil
	}

	// A generated name must never silently replace an existing definition.
	var listing struct {
		Items []model.Definition `json:"items"`
	}
	if err := requestJSON("GET", "/api/v1/jobs", nil, &listing); err != nil {
		return err
	}
	for _, d := range listing.Items {
		for _, entry := range entries {
			if d.Name == entry.job.Name {
				return fmt.Errorf("job %q already exists in minicrond; resolve the conflict before importing", d.Name)
			}
		}
	}
	bundle := struct {
		Jobs []model.Definition `toml:"job"`
	}{}
	for _, entry := range entries {
		bundle.Jobs = append(bundle.Jobs, entry.job)
	}
	content, err := toml.Marshal(bundle)
	if err != nil {
		return err
	}
	request := map[string]string{"content": string(content)}
	var preview struct {
		ContentHash string `json:"content_hash"`
	}
	if err := requestJSON("POST", "/api/v1/import/preview", request, &preview); err != nil {
		return err
	}
	fmt.Printf("The following jobs will be imported into the selected daemon and commented out in %s's crontab:\n", cronOwner(*userName))
	for _, entry := range entries {
		fmt.Printf("  line %d -> %s: %s\n", entry.line, entry.job.Name, entry.text)
	}
	fmt.Print("Press Enter to import, or type anything to cancel: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("confirmation: %w", err)
	}
	if strings.TrimSpace(answer) != "" {
		fmt.Println("cancelled; crontab unchanged")
		return nil
	}
	// Abort if another editor changed the crontab during the preview.
	current, err := readCrontab(*userName)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return errors.New("crontab changed since preview; nothing imported")
	}
	request["hash"] = preview.ContentHash
	var result struct {
		Applied int `json:"applied"`
	}
	if err := requestJSON("POST", "/api/v1/import/apply", request, &result); err != nil {
		return err
	}
	if result.Applied != len(entries) {
		return fmt.Errorf("daemon reported %d imported jobs, expected %d; crontab left unchanged", result.Applied, len(entries))
	}
	current, err = readCrontab(*userName)
	if err != nil || !bytes.Equal(current, original) {
		return fmt.Errorf("imported %d jobs, but crontab changed or could not be re-read; comment out the original entries manually (read error: %v)", result.Applied, err)
	}
	updated := commentCrontab(original, entries)
	cmd := crontabCommand(*userName, "-")
	cmd.Stdin = bytes.NewReader(updated)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("imported %d jobs, but failed to comment out crontab (jobs may run twice); edit it manually: %w: %s", result.Applied, err, strings.TrimSpace(string(output)))
	}
	fmt.Printf("imported %d jobs and commented out their crontab entries\n", result.Applied)
	return nil
}

func cronOwner(userName string) string {
	if userName != "" {
		return userName
	}
	return "current user"
}

func crontabCommand(userName, operation string) *exec.Cmd {
	if userName != "" {
		return exec.Command("crontab", "-u", userName, operation)
	}
	return exec.Command("crontab", operation)
}

func readCrontab(userName string) ([]byte, error) {
	cmd := crontabCommand(userName, "-l")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading %s's crontab: %w", cronOwner(userName), err)
	}
	return out, nil
}

func parseCrontab(content []byte) ([]cronEntry, error) {
	var entries []cronEntry
	env := map[string]string{}
	shell := "/bin/sh"
	for i, raw := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := cronAssignment.FindStringSubmatch(line); m != nil {
			value := m[2]
			if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
				value = value[1 : len(value)-1]
			}
			if m[1] == "CRON_TZ" || m[1] == "TZ" {
				return nil, fmt.Errorf("crontab line %d: timezone variables need manual migration", i+1)
			}
			if m[1] == "SHELL" {
				if !strings.HasPrefix(value, "/") {
					return nil, fmt.Errorf("crontab line %d: SHELL must be an absolute path", i+1)
				}
				shell = value
			} else {
				env[m[1]] = value
			}
			continue
		}
		var schedule, command string
		if strings.HasPrefix(line, "@") {
			parts := strings.Fields(line)
			if len(parts) < 2 || parts[0] == "@reboot" {
				return nil, fmt.Errorf("crontab line %d: unsupported cron directive", i+1)
			}
			schedule = parts[0]
			command = strings.TrimSpace(strings.TrimPrefix(line, schedule))
		} else {
			parts := strings.Fields(line)
			if len(parts) < 6 {
				return nil, fmt.Errorf("crontab line %d: expected five fields and a command", i+1)
			}
			schedule = strings.Join(parts[:5], " ")
			// Preserve all command whitespace and quoting after the fifth field.
			rest := line
			for range 5 {
				rest = strings.TrimSpace(rest)
				rest = rest[len(strings.Fields(rest)[0]):]
			}
			command = strings.TrimSpace(rest)
		}
		if strings.Contains(command, "%") {
			return nil, fmt.Errorf("crontab line %d: %% stdin syntax is not supported; crontab unchanged", i+1)
		}
		// Line number distinguishes identical entries while retaining a stable name on retries.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", i+1, raw)))
		job := model.Definition{Name: "crontab-" + hex.EncodeToString(sum[:8]), Kind: model.KindJob, Schedule: schedule, Command: command, Shell: shell, Timezone: time.Local.String(), Env: make(map[string]string, len(env))}
		for k, v := range env {
			job.Env[k] = v
		}
		entries = append(entries, cronEntry{line: i + 1, text: raw, job: job})
	}
	return entries, nil
}

func commentCrontab(original []byte, entries []cronEntry) []byte {
	lines := bytes.SplitAfter(original, []byte("\n"))
	for _, entry := range entries {
		lines[entry.line-1] = append([]byte("# minicrond imported: "), lines[entry.line-1]...)
	}
	return bytes.Join(lines, nil)
}
