package hosts_test

// Test plan (AGENTS.md section 5 and T18's Contract, copied verbatim before
// any test below was written):
//
//  1. A host matching an existing rule connects without prompting.
//  2. A failed probe writes nothing to config.toml and surfaces the error.
//  3. A successful probe writes the new profile.
//  4. Promoting a match rule leaves an existing hand-written comment in
//     config.toml intact.
//  5. Delete and pin update state and are reflected in the list ordering.
//
// Every test below feeds a tea.Msg to Update and asserts on the returned
// Model's exported accessors (Hosts) and on the tea.Cmd it emitted -- run by
// calling it directly, the same way browser_test.go and library_test.go do
// -- never on View's rendered output (DESIGN.md section 13).
//
// Mechanic 4 is tested twice, deliberately, because it is the one AGENTS.md
// section 9 calls out by name as the trap this whole task exists to close:
// go-toml/v2 has no comment-preserving round trip, so a config writer that
// re-marshals instead of editing silently destroys every hand-written
// comment. TestApplyFirstRun_PreservesHandWrittenComment exercises
// hosts.ApplyFirstRun directly, the pure textual editor, and asserts the
// hand-written comment in testdata/with_comment.toml -- one a naive
// toml.Marshal(cfg) would drop, since it is not represented in the Config
// struct at all -- is byte-for-byte still there, with the original bytes an
// exact prefix of the result.
// TestModel_PromotingMatchRulePreservesHandWrittenComment exercises the same
// guarantee through the full path a real run takes: Model.Update driving
// hosts.WriteConfigFile against a real file on disk. Either one regressing
// to toml.Marshal would fail both.
//
// Every fixture in testdata/ is copied into t.TempDir() before a test writes
// to it (copyFixture below); no test ever writes to the committed fixture or
// to a real XDG config path.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/hosts"
)

// update drives m through one Update call and type-asserts the returned
// ui.ScreenModel back to hosts.Model, which is what every test in this file
// needs to keep chaining calls the way real usage does.
func update(t *testing.T, m hosts.Model, msg tea.Msg) (hosts.Model, tea.Cmd) {
	t.Helper()
	screen, cmd := m.Update(msg)
	hm, ok := screen.(hosts.Model)
	if !ok {
		t.Fatalf("Update(%T) returned %T, want hosts.Model", msg, screen)
	}
	return hm, cmd
}

// typeString drives m through Update once per rune of s, the same way real
// key presses arrive: one tea.KeyMsg of type KeyRunes per character
// (picker_test.go and browser_test.go use the identical pattern).
func typeString(t *testing.T, m hosts.Model, s string) hosts.Model {
	t.Helper()
	for _, r := range s {
		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// copyFixture copies testdata/name into a fresh t.TempDir() as config.toml,
// loads it into a *config.Config the same way production wiring would, and
// returns the path, the parsed config and the fixture's original bytes for
// later before/after comparison. It never reads or writes the committed
// fixture in place.
func copyFixture(t *testing.T, name string) (path string, cfg *config.Config, original []byte) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture testdata/%s: %v", name, err)
	}

	path = filepath.Join(t.TempDir(), "config.toml")
	if werr := os.WriteFile(path, data, 0o600); werr != nil {
		t.Fatalf("seeding %s: %v", path, werr)
	}

	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("loading seeded config %s: %v", path, err)
	}

	return path, cfg, data
}

// failIfCalledList returns a ListFunc that fails the test if it is ever
// invoked, for tests that drive the model without calling Init.
func failIfCalledList(t *testing.T) hosts.ListFunc {
	t.Helper()
	return func() ([]ui.HostSummary, error) {
		t.Fatalf("ListFunc was called; this test drives the model directly without calling Init")
		return nil, nil
	}
}

// failIfCalledProber returns a ProbeFunc that fails the test if it is ever
// invoked, for asserting a path that must connect without prompting
// (mechanic 1).
func failIfCalledProber(t *testing.T) hosts.ProbeFunc {
	t.Helper()
	return func(ctx context.Context, host string, exec []string) error {
		t.Fatalf("ProbeFunc was called for host %q; a host resolved from an existing rule must connect without probing", host)
		return nil
	}
}

// failIfCalledWriter returns a WriteFunc that fails the test if it is ever
// invoked, which is how mechanic 2 proves nothing was written to config.toml
// after a failed probe (see WriteFunc's own doc comment in hosts.go).
func failIfCalledWriter(t *testing.T) hosts.WriteFunc {
	t.Helper()
	return func(host, profileName string, profile config.Profile) error {
		t.Fatalf("WriteFunc was called for host %q, profile %q; a failed probe must never write to config.toml", host, profileName)
		return nil
	}
}

// recordingPinner is a PinFunc test double that records every call it
// receives and always succeeds.
type recordingPinner struct {
	calls []pinCall
}

type pinCall struct {
	Host   string
	Pinned bool
}

func (p *recordingPinner) pin(host string, pinned bool) error {
	p.calls = append(p.calls, pinCall{Host: host, Pinned: pinned})
	return nil
}

// recordingRemover is a RemoveFunc test double that records every host it
// was asked to remove and always succeeds.
type recordingRemover struct {
	calls []string
}

func (r *recordingRemover) remove(host string) error {
	r.calls = append(r.calls, host)
	return nil
}

// --- Mechanic 1: a host matching an existing rule connects without prompting

func TestModel_FirstRunGuidance(t *testing.T) {
	m := hosts.New(nil, failIfCalledList(t))
	m, _ = update(t, m, ui.HostsLoadedMsg{})
	if got := m.View(); !strings.Contains(got, "New connection") || !strings.Contains(got, "Host (user@hostname)") || !strings.Contains(got, "Identity file") {
		t.Fatalf("first-run form is missing connection guidance:\n%s", got)
	}

	const host = "example-host"
	m = typeString(t, m, host)
	if got := m.View(); !strings.Contains(got, host) || !strings.Contains(got, "Enter connect") {
		t.Fatalf("first-run form does not show the entered host and next action:\n%s", got)
	}
}

func TestModel_FirstRunIdentityIsOptional(t *testing.T) {
	const host = "agent-backed.example.com"
	var gotExec []string
	prober := func(_ context.Context, _ string, exec []string) error {
		gotExec = append([]string(nil), exec...)
		return nil
	}
	m := hosts.New(nil, failIfCalledList(t), hosts.WithProber(prober))
	m, _ = update(t, m, ui.HostsLoadedMsg{})
	m = typeString(t, m, host)
	_, probeCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if probeCmd == nil {
		t.Fatal("blank identity returned nil probe command")
	}
	_ = probeCmd()
	if diff := cmp.Diff(config.DefaultProfile.Exec, gotExec); diff != "" {
		t.Fatalf("blank identity changed the default SSH template (-want +got):\n%s", diff)
	}
}

func TestModel_ThemeEditorPreviewsAndSavesHexPalette(t *testing.T) {
	var savedPrimary, savedSecondary string
	saver := func(primary, secondary string) error {
		savedPrimary, savedSecondary = primary, secondary
		return nil
	}
	m := hosts.New(nil, failIfCalledList(t), hosts.WithThemeSaver(saver))
	m, _ = update(t, m, ui.HostsLoadedMsg{Hosts: []ui.HostSummary{{Name: "example.com", Profile: "default"}}})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.Theming() {
		t.Fatal("ctrl+t did not open the theme editor")
	}

	var previewCmd tea.Cmd
	for _, r := range "#112233" {
		m, previewCmd = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if previewCmd == nil {
		t.Fatal("completing the primary hex color returned no live-preview command")
	}
	preview, ok := previewCmd().(ui.ThemeChangedMsg)
	if !ok || preview.Primary != "#112233" {
		t.Fatalf("primary preview = %#v, want #112233", preview)
	}
	m, _ = update(t, m, preview)

	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	for _, r := range "#ffaa00" {
		m, previewCmd = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	preview, ok = previewCmd().(ui.ThemeChangedMsg)
	if !ok || preview.Secondary != "#ffaa00" {
		t.Fatalf("secondary preview = %#v, want #ffaa00", preview)
	}
	m, _ = update(t, m, preview)

	m, saveCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if saveCmd == nil {
		t.Fatal("theme Enter returned nil save command")
	}
	m, _ = update(t, m, saveCmd())
	if m.Theming() {
		t.Fatal("successful theme save left the editor open")
	}
	if savedPrimary != "#112233" || savedSecondary != "#ffaa00" {
		t.Fatalf("saved palette = %q/%q", savedPrimary, savedSecondary)
	}
}

func TestModel_FirstRunFormBuildsAuthenticatedExecTemplate(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("writing identity fixture: %v", err)
	}
	const host = "ec2-user@example.com"
	var gotExec []string
	prober := func(_ context.Context, gotHost string, exec []string) error {
		if gotHost != host {
			t.Fatalf("probe host = %q, want %q", gotHost, host)
		}
		gotExec = append([]string(nil), exec...)
		return nil
	}

	configPath := filepath.Join(t.TempDir(), "config.toml")
	m := hosts.New(nil, failIfCalledList(t),
		hosts.WithProber(prober),
		hosts.WithWriter(hosts.WriteConfigFile(configPath)),
	)
	m, _ = update(t, m, ui.HostsLoadedMsg{})
	m = typeString(t, m, host)
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeString(t, m, keyPath)
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeString(t, m, "ssh -o BatchMode=yes")
	m, probeCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if probeCmd == nil {
		t.Fatal("connection form returned nil probe command")
	}
	probeResult := probeCmd()

	want := []string{"ssh", "-o", "BatchMode=yes", "-i", keyPath, "{host}", "--", "{cmd}"}
	if diff := cmp.Diff(want, gotExec); diff != "" {
		t.Fatalf("probe exec template mismatch (-want +got):\n%s", diff)
	}

	_, writeCmd := update(t, m, probeResult)
	if writeCmd == nil {
		t.Fatal("successful probe returned nil config write command")
	}
	if _, ok := writeCmd().(ui.HostSelectedMsg); !ok {
		t.Fatal("config write did not continue to host selection")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("loading generated config: %v", err)
	}
	if diff := cmp.Diff(want, cfg.Profiles[host].Exec); diff != "" {
		t.Fatalf("persisted exec template mismatch (-want +got):\n%s", diff)
	}
}

func TestModel_FirstRunRejectsOpenIdentityPermissionsBeforeProbe(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "open.pem")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("writing identity fixture: %v", err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("opening identity permissions: %v", err)
	}

	m := hosts.New(nil, failIfCalledList(t), hosts.WithProber(failIfCalledProber(t)))
	m, _ = update(t, m, ui.HostsLoadedMsg{})
	m = typeString(t, m, "ec2-user@example.com")
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeString(t, m, keyPath)
	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("open identity permissions returned nil error command")
	}
	msg := cmd()
	errMsg, ok := msg.(ui.ErrorMsg)
	if !ok {
		t.Fatalf("open identity permissions returned %T, want ui.ErrorMsg", msg)
	}
	if got := errMsg.Err.Error(); !strings.Contains(got, "0644") || !strings.Contains(got, "chmod 600") {
		t.Fatalf("identity permission error is not actionable: %s", got)
	}
}

func TestModel_HostMatchingExistingRuleConnectsWithoutPrompting(t *testing.T) {
	t.Run("typing into an empty picker and pressing enter commits the hostname", func(t *testing.T) {
		cfg := &config.Config{
			Profiles: map[string]config.Profile{
				"corp": {Exec: []string{"ssh", "{host}", "--", "{cmd}"}},
			},
			Matches: []config.MatchRule{{Host: "*.compute.amazonaws.com", Profile: "corp"}},
		}
		const host = "ec2-13-244-248-216.af-south-1.compute.amazonaws.com"

		m := hosts.New(cfg, failIfCalledList(t),
			hosts.WithProber(failIfCalledProber(t)),
			hosts.WithWriter(failIfCalledWriter(t)),
		)
		m = typeString(t, m, host)

		_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("Enter on a hostname typed into an empty picker returned nil Cmd")
		}
		msg := cmd()
		got, ok := msg.(ui.HostSelectedMsg)
		if !ok {
			t.Fatalf("Enter reported %T, want ui.HostSelectedMsg", msg)
		}
		want := ui.HostSelectedMsg{Host: host, Profile: "corp"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("HostSelectedMsg mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("an existing history row commits without probing or writing", func(t *testing.T) {
		summary := ui.HostSummary{
			Name:     "jenkins-01.prod.internal",
			Profile:  "corp",
			LastSeen: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		}

		m := hosts.New(&config.Config{}, failIfCalledList(t),
			hosts.WithProber(failIfCalledProber(t)),
			hosts.WithWriter(failIfCalledWriter(t)),
		)

		m, loadCmd := update(t, m, ui.HostsLoadedMsg{Hosts: []ui.HostSummary{summary}})
		if loadCmd != nil {
			t.Fatalf("Update(HostsLoadedMsg) returned a non-nil Cmd, want nil")
		}

		_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Fatalf("Update(enter on an existing row) returned a nil Cmd, want the command reporting ui.HostSelectedMsg")
		}

		msg := cmd()
		selected, ok := msg.(ui.HostSelectedMsg)
		if !ok {
			t.Fatalf("enter on an existing row reported %T, want ui.HostSelectedMsg", msg)
		}
		if diff := cmp.Diff(ui.HostSelectedMsg{Host: summary.Name, Profile: summary.Profile}, selected); diff != "" {
			t.Fatalf("HostSelectedMsg (-want +got):\n%s", diff)
		}
	})

	t.Run("a new hostname matching a config rule commits without probing or writing", func(t *testing.T) {
		cfg := &config.Config{
			Profiles: map[string]config.Profile{
				"corp": {Exec: []string{"ssh", "{host}", "--", "{cmd}"}},
			},
			Matches: []config.MatchRule{
				{Host: "*.prod.internal", Profile: "corp"},
			},
		}
		const host = "web-03.prod.internal"

		m := hosts.New(cfg, failIfCalledList(t),
			hosts.WithProber(failIfCalledProber(t)),
			hosts.WithWriter(failIfCalledWriter(t)),
		)

		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
		m = typeString(t, m, host)
		_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Fatalf("Update(enter on a hostname matching an existing rule) returned a nil Cmd, want the command reporting ui.HostSelectedMsg")
		}

		msg := cmd()
		selected, ok := msg.(ui.HostSelectedMsg)
		if !ok {
			t.Fatalf("enter on a hostname matching an existing rule reported %T, want ui.HostSelectedMsg", msg)
		}
		if diff := cmp.Diff(ui.HostSelectedMsg{Host: host, Profile: "corp"}, selected); diff != "" {
			t.Fatalf("HostSelectedMsg (-want +got):\n%s", diff)
		}
	})
}

// --- Mechanic 2: a failed probe writes nothing to config.toml and surfaces
// the error

func TestModel_FailedProbeWritesNothingAndSurfacesError(t *testing.T) {
	t.Run("a failed probe writes nothing to config.toml and surfaces the error", func(t *testing.T) {
		path, cfg, original := copyFixture(t, "base.toml")
		const host = "widget-07.staging.internal"

		probeErr := errors.New("dial tcp 10.0.0.9:22: connect: connection refused")
		prober := func(ctx context.Context, gotHost string, exec []string) error {
			if gotHost != host {
				t.Fatalf("ProbeFunc called with host %q, want %q", gotHost, host)
			}
			return probeErr
		}

		m := hosts.New(cfg, failIfCalledList(t),
			hosts.WithProber(prober),
			hosts.WithWriter(failIfCalledWriter(t)),
		)

		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
		m = typeString(t, m, host)
		m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Fatalf("Update(enter on a genuinely new host) returned a nil Cmd, want the probe command")
		}

		m, errCmd := update(t, m, cmd())
		if errCmd == nil {
			t.Fatal("failed probe result returned nil Cmd, want ui.ErrorMsg command")
		}
		errResult := errCmd()
		errMsg, ok := errResult.(ui.ErrorMsg)
		if !ok {
			t.Fatalf("probe command reported %T, want ui.ErrorMsg", errResult)
		}
		if !errors.Is(errMsg.Err, probeErr) {
			t.Fatalf("ErrorMsg.Err = %v, want it to wrap %v", errMsg.Err, probeErr)
		}
		if !strings.Contains(errMsg.Err.Error(), host) {
			t.Fatalf("ErrorMsg.Err = %q, want it to name the host %q", errMsg.Err, host)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s after the failed probe: %v", path, err)
		}
		if !bytes.Equal(original, got) {
			t.Fatalf("config.toml changed after a failed probe:\nbefore:\n%s\nafter:\n%s", original, got)
		}
	})
}

// --- Mechanic 3: a successful probe writes the new profile

func TestWriteConfigFileCreatesMissingParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logpick", "config.toml")
	const host = "example-host"

	if err := hosts.WriteConfigFile(path)(host, host, config.DefaultProfile); err != nil {
		t.Fatalf("WriteConfigFile(%s): %v", path, err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("loading newly created config: %v", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat config directory: %v", err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("config directory permissions = %#o, want no group/world access", got)
	}
}

func TestModel_SuccessfulProbeWritesNewProfile(t *testing.T) {
	t.Run("a successful probe writes the new profile and match rule", func(t *testing.T) {
		path, cfg, _ := copyFixture(t, "base.toml")
		const host = "widget-07.staging.internal"

		prober := func(ctx context.Context, gotHost string, exec []string) error {
			if gotHost != host {
				t.Fatalf("ProbeFunc called with host %q, want %q", gotHost, host)
			}
			if diff := cmp.Diff(config.DefaultProfile.Exec, exec); diff != "" {
				t.Fatalf("ProbeFunc exec (-want +got):\n%s", diff)
			}
			return nil
		}

		m := hosts.New(cfg, failIfCalledList(t),
			hosts.WithProber(prober),
			hosts.WithWriter(hosts.WriteConfigFile(path)),
		)

		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
		m = typeString(t, m, host)
		m, probeCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if probeCmd == nil {
			t.Fatalf("Update(enter on a genuinely new host) returned a nil Cmd, want the probe command")
		}

		_, writeCmd := update(t, m, probeCmd())
		if writeCmd == nil {
			t.Fatalf("successful probe result returned nil Cmd, want the write command")
		}

		selectedMsg := writeCmd()
		selected, ok := selectedMsg.(ui.HostSelectedMsg)
		if !ok {
			t.Fatalf("write command reported %T, want ui.HostSelectedMsg", selectedMsg)
		}
		if diff := cmp.Diff(ui.HostSelectedMsg{Host: host, Profile: host}, selected); diff != "" {
			t.Fatalf("HostSelectedMsg (-want +got):\n%s", diff)
		}

		gotCfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("reloading %s after the write: %v", path, err)
		}

		wantProfile := config.Profile{Exec: config.DefaultProfile.Exec, Scan: config.DefaultProfile.Scan}
		if diff := cmp.Diff(wantProfile, gotCfg.Profiles[host]); diff != "" {
			t.Fatalf("written profile (-want +got):\n%s", diff)
		}

		wantMatches := append(append([]config.MatchRule{}, cfg.Matches...), config.MatchRule{Host: host, Profile: host})
		if diff := cmp.Diff(wantMatches, gotCfg.Matches); diff != "" {
			t.Fatalf("match rules after the write (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff(cfg.Profiles["corp"], gotCfg.Profiles["corp"]); diff != "" {
			t.Fatalf("pre-existing profile changed by the write (-want +got):\n%s", diff)
		}
	})
}

// --- Mechanic 4: promoting a match rule leaves an existing hand-written
// comment in config.toml intact

func TestApplyFirstRun_PreservesHandWrittenComment(t *testing.T) {
	t.Run("promoting a match rule leaves an existing hand-written comment in config.toml intact", func(t *testing.T) {
		original, err := os.ReadFile(filepath.Join("testdata", "with_comment.toml"))
		if err != nil {
			t.Fatalf("reading testdata/with_comment.toml: %v", err)
		}
		const host = "abc.example.com"

		got, err := hosts.ApplyFirstRun(original, host, host, config.DefaultProfile)
		if err != nil {
			t.Fatalf("ApplyFirstRun: %v", err)
		}

		if !bytes.HasPrefix(got, original) {
			t.Fatalf("ApplyFirstRun's result does not start with the original bytes verbatim; a naive toml.Marshal round trip would silently drop the hand-written comment.\noriginal:\n%s\ngot:\n%s", original, got)
		}

		const wantComment = "# Personal note: jenkins-01 is flaky right after 2am, don't page on it."
		if !strings.Contains(string(got), wantComment) {
			t.Fatalf("ApplyFirstRun's result no longer contains the hand-written comment %q:\n%s", wantComment, got)
		}

		// The header key must be quoted. An unquoted [profile.a.b.c] is
		// three nested tables to TOML, not one key "a.b.c", so a dotted
		// hostname only round-trips back through config.Load in the
		// quoted form.
		wantHeader := "[profile." + strconv.Quote(host) + "]"
		if !strings.Contains(string(got), wantHeader) {
			t.Fatalf("ApplyFirstRun's result is missing the new profile block %s for %q:\n%s", wantHeader, host, got)
		}
	})
}

func TestModel_PromotingMatchRulePreservesHandWrittenComment(t *testing.T) {
	t.Run("promoting a match rule leaves an existing hand-written comment in config.toml intact", func(t *testing.T) {
		path, cfg, original := copyFixture(t, "with_comment.toml")
		const host = "abc.example.com"

		prober := func(ctx context.Context, gotHost string, exec []string) error { return nil }

		m := hosts.New(cfg, failIfCalledList(t),
			hosts.WithProber(prober),
			hosts.WithWriter(hosts.WriteConfigFile(path)),
		)

		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
		m = typeString(t, m, host)
		m, probeCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if probeCmd == nil {
			t.Fatalf("Update(enter on a genuinely new host) returned a nil Cmd, want the probe command")
		}

		_, writeCmd := update(t, m, probeCmd())
		if writeCmd == nil {
			t.Fatalf("successful probe result returned nil Cmd, want the write command")
		}

		selectedMsg := writeCmd()
		if _, ok := selectedMsg.(ui.HostSelectedMsg); !ok {
			t.Fatalf("write command reported %T, want ui.HostSelectedMsg", selectedMsg)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s after the write: %v", path, err)
		}

		if !bytes.HasPrefix(got, original) {
			t.Fatalf("config.toml's original bytes were not preserved verbatim; the writer must edit config.toml in place, never re-marshal it.\noriginal:\n%s\ngot:\n%s", original, got)
		}

		const wantComment = "# Personal note: jenkins-01 is flaky right after 2am, don't page on it."
		if !strings.Contains(string(got), wantComment) {
			t.Fatalf("config.toml no longer contains the hand-written comment after promoting a match rule:\n%s", got)
		}
	})
}

// --- Mechanic 5: delete and pin update state and are reflected in the list
// ordering

func TestModel_DeleteAndPinUpdateOrdering(t *testing.T) {
	t1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	alpha := ui.HostSummary{Name: "alpha.example.com", Profile: "corp", LastSeen: t1}
	bravo := ui.HostSummary{Name: "bravo.example.com", Profile: "corp", LastSeen: t2}
	charlie := ui.HostSummary{Name: "charlie.example.com", Profile: "corp", LastSeen: t3}

	// sorted is the display order Init's own sort would produce for these
	// three, none pinned: most recently seen first (DESIGN.md 9.1).
	sorted := []ui.HostSummary{charlie, bravo, alpha}

	t.Run("pin promotes a host to the top of the list", func(t *testing.T) {
		pinner := &recordingPinner{}
		m := hosts.New(&config.Config{}, failIfCalledList(t), hosts.WithPinner(pinner.pin))

		m, _ = update(t, m, ui.HostsLoadedMsg{Hosts: sorted})

		// Isolate the oldest host in the match set so ctrl+p, which acts on
		// the row under the cursor, targets it rather than whatever sorted
		// first.
		m = typeString(t, m, "alpha")

		m, pinCmd := update(t, m, tea.KeyMsg{Type: tea.KeyCtrlP})
		if pinCmd == nil {
			t.Fatalf("Update(ctrl+p) returned a nil Cmd, want the pin command")
		}

		msg := pinCmd()
		pinned, ok := msg.(ui.HostPinnedMsg)
		if !ok {
			t.Fatalf("pin command reported %T, want ui.HostPinnedMsg", msg)
		}
		if diff := cmp.Diff(ui.HostPinnedMsg{Host: alpha.Name, Pinned: true}, pinned); diff != "" {
			t.Fatalf("HostPinnedMsg (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]pinCall{{Host: alpha.Name, Pinned: true}}, pinner.calls); diff != "" {
			t.Fatalf("pinner calls (-want +got):\n%s", diff)
		}

		m, applyCmd := update(t, m, pinned)
		if applyCmd != nil {
			t.Fatalf("Update(HostPinnedMsg) returned a non-nil Cmd, want nil")
		}

		wantAlpha := alpha
		wantAlpha.Pinned = true
		want := []ui.HostSummary{wantAlpha, charlie, bravo}
		if diff := cmp.Diff(want, m.Hosts()); diff != "" {
			t.Fatalf("Hosts() after pinning (-want +got):\n%s", diff)
		}
	})

	t.Run("delete removes a host and keeps the rest in order", func(t *testing.T) {
		remover := &recordingRemover{}
		m := hosts.New(&config.Config{}, failIfCalledList(t), hosts.WithRemover(remover.remove))

		m, _ = update(t, m, ui.HostsLoadedMsg{Hosts: sorted})

		// Isolate the middle host so ctrl+d, which acts on the row under
		// the cursor, targets it rather than whatever sorted first.
		m = typeString(t, m, "bravo")

		m, delCmd := update(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
		if delCmd == nil {
			t.Fatalf("Update(ctrl+d) returned a nil Cmd, want the delete command")
		}

		msg := delCmd()
		removed, ok := msg.(ui.HostRemovedMsg)
		if !ok {
			t.Fatalf("delete command reported %T, want ui.HostRemovedMsg", msg)
		}
		if diff := cmp.Diff(ui.HostRemovedMsg{Host: bravo.Name}, removed); diff != "" {
			t.Fatalf("HostRemovedMsg (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{bravo.Name}, remover.calls); diff != "" {
			t.Fatalf("remover calls (-want +got):\n%s", diff)
		}

		m, applyCmd := update(t, m, removed)
		if applyCmd != nil {
			t.Fatalf("Update(HostRemovedMsg) returned a non-nil Cmd, want nil")
		}

		want := []ui.HostSummary{charlie, alpha}
		if diff := cmp.Diff(want, m.Hosts()); diff != "" {
			t.Fatalf("Hosts() after deleting (-want +got):\n%s", diff)
		}
	})
}
