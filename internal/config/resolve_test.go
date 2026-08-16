package config

import (
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Test plan for T07 (DESIGN.md section 6.4, AGENTS.md section 5). Copied
// verbatim from the task before any test below was written.
//
// Mechanics:
//  1. Each precedence level wins over the one below it: CLI over host
//     entry, host entry over match rule, match rule over profile, profile
//     over default.
//  2. The first matching [[match]] in file order wins when several match.
//  3. ScanSpec fields merge individually, so overriding paths keeps the
//     profile's include.
//  4. A host matching nothing resolves to the default profile.
//  5. A host entry naming an undefined profile is an error.

// ptr returns the address of v, for building the pointer fields of
// Overrides. A nil field means "flag absent", a non-nil field means "flag
// given", including when the value it points at is empty.
func ptr[T any](v T) *T {
	return &v
}

// resolveCase is one row of a table asserting on a successful resolution.
type resolveCase struct {
	name string
	cfg  *Config
	host string
	ov   Overrides
	want ResolvedHost
}

// runResolveCases resolves every case and compares the result with go-cmp.
func runResolveCases(t *testing.T, cases []resolveCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.cfg, tc.host, tc.ov)
			if err != nil {
				t.Fatalf("Resolve(cfg, %q, %+v) returned an unexpected error: %v", tc.host, tc.ov, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Resolve(cfg, %q, %+v) mismatch (-want +got):\n%s", tc.host, tc.ov, diff)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Run("each precedence level wins over the one below it", func(t *testing.T) {
		runResolveCases(t, []resolveCase{
			{
				name: "CLI flags win over the host entry",
				cfg: &Config{
					Profiles: map[string]Profile{
						"corp": {
							Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
							Scan: ScanSpec{Paths: []string{"/corp"}, Include: []string{"*.corp"}},
						},
						"cli": {
							Exec: []string{"ssh", "{host}", "--", "{cmd}"},
							Scan: ScanSpec{Paths: []string{"/cli"}, Include: []string{"*.cli"}},
						},
					},
					Hosts: map[string]HostEntry{
						"jenkins-01": {
							Profile: "corp",
							Scan:    ScanSpec{Paths: []string{"/host"}},
							Label:   "CI primary",
						},
					},
				},
				host: "jenkins-01",
				ov:   Overrides{Profile: ptr("cli"), Paths: ptr([]string{"/flag"})},
				want: ResolvedHost{
					Name:        "jenkins-01",
					ProfileName: "cli",
					Profile: Profile{
						Exec: []string{"ssh", "{host}", "--", "{cmd}"},
						Scan: ScanSpec{
							Paths:    []string{"/flag"},
							MaxDepth: DefaultProfile.Scan.MaxDepth,
							Include:  []string{"*.cli"},
							Exclude:  DefaultProfile.Scan.Exclude,
						},
					},
					Label: "CI primary",
				},
			},
			{
				name: "the host entry wins over the match rule",
				cfg: &Config{
					Profiles: map[string]Profile{
						"corp": {
							Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
							Scan: ScanSpec{Paths: []string{"/corp"}, Include: []string{"*.corp"}},
						},
						"web": {
							Exec: []string{"ssh", "{host}", "--", "{cmd}"},
							Scan: ScanSpec{Paths: []string{"/web"}, Include: []string{"*.web"}},
						},
					},
					Matches: []MatchRule{
						{Host: "jenkins-*", Profile: "web", Scan: ScanSpec{Paths: []string{"/rule"}}},
					},
					Hosts: map[string]HostEntry{
						"jenkins-01": {
							Profile: "corp",
							Scan:    ScanSpec{Paths: []string{"/host"}},
							Label:   "CI primary",
						},
					},
				},
				host: "jenkins-01",
				want: ResolvedHost{
					Name:        "jenkins-01",
					ProfileName: "corp",
					Profile: Profile{
						Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
						Scan: ScanSpec{
							Paths:    []string{"/host"},
							MaxDepth: DefaultProfile.Scan.MaxDepth,
							Include:  []string{"*.corp"},
							Exclude:  DefaultProfile.Scan.Exclude,
						},
					},
					Label: "CI primary",
				},
			},
			{
				name: "the match rule wins over the profile",
				cfg: &Config{
					Profiles: map[string]Profile{
						"corp": {
							Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
							Scan: ScanSpec{
								Paths:    []string{"/corp"},
								MaxDepth: 5,
								Include:  []string{"*.corp"},
							},
						},
					},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp", Scan: ScanSpec{Paths: []string{"/rule"}}},
					},
				},
				host: "web-01.prod.internal",
				want: ResolvedHost{
					Name:        "web-01.prod.internal",
					ProfileName: "corp",
					Profile: Profile{
						Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
						Scan: ScanSpec{
							Paths:    []string{"/rule"},
							MaxDepth: 5,
							Include:  []string{"*.corp"},
							Exclude:  DefaultProfile.Scan.Exclude,
						},
					},
				},
			},
			{
				name: "the profile wins over the built-in defaults",
				cfg: &Config{
					Profiles: map[string]Profile{
						"corp": {
							Exec:       []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
							Persistent: true,
							AuthHint:   "corp-bastion login",
							Scan:       ScanSpec{Paths: []string{"/opt/app/logs"}, MaxDepth: 5},
						},
					},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp"},
					},
				},
				host: "web-01.prod.internal",
				want: ResolvedHost{
					Name:        "web-01.prod.internal",
					ProfileName: "corp",
					Profile: Profile{
						Exec:       []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
						Persistent: true,
						AuthHint:   "corp-bastion login",
						Scan: ScanSpec{
							Paths:    []string{"/opt/app/logs"},
							MaxDepth: 5,
							Include:  DefaultProfile.Scan.Include,
							Exclude:  DefaultProfile.Scan.Exclude,
						},
					},
				},
			},
		})
	})

	t.Run("the first matching match rule in file order wins", func(t *testing.T) {
		profiles := map[string]Profile{
			"first":  {Exec: []string{"first", "{host}", "{cmd}"}},
			"second": {Exec: []string{"second", "{host}", "{cmd}"}},
			"third":  {Exec: []string{"third", "{host}", "{cmd}"}},
		}

		runResolveCases(t, []resolveCase{
			{
				name: "the earliest of several matching rules wins",
				cfg: &Config{
					Profiles: profiles,
					Matches: []MatchRule{
						{Host: "*.internal", Profile: "first"},
						{Host: "*.prod.internal", Profile: "second"},
						{Host: "*", Profile: "third"},
					},
				},
				host: "web-01.prod.internal",
				want: ResolvedHost{
					Name:        "web-01.prod.internal",
					ProfileName: "first",
					Profile: Profile{
						Exec: []string{"first", "{host}", "{cmd}"},
						Scan: DefaultProfile.Scan,
					},
				},
			},
			{
				name: "a wildcard spans dots, so a multi-label host still matches",
				cfg: &Config{
					Profiles: profiles,
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "second"},
					},
				},
				host: "a.b.prod.internal",
				want: ResolvedHost{
					Name:        "a.b.prod.internal",
					ProfileName: "second",
					Profile: Profile{
						Exec: []string{"second", "{host}", "{cmd}"},
						Scan: DefaultProfile.Scan,
					},
				},
			},
			{
				name: "a wildcard does not match an absent label",
				cfg: &Config{
					Profiles: profiles,
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "second"},
					},
				},
				host: "prod.internal",
				want: ResolvedHost{
					Name:        "prod.internal",
					ProfileName: DefaultProfileName,
					Profile:     DefaultProfile,
				},
			},
		})

		t.Run("a malformed pattern is an error naming the pattern", func(t *testing.T) {
			cfg := &Config{
				Profiles: profiles,
				Matches: []MatchRule{
					{Host: "[", Profile: "first"},
					{Host: "*", Profile: "third"},
				},
			}

			_, err := Resolve(cfg, "web-01.prod.internal", Overrides{})
			if !errors.Is(err, path.ErrBadPattern) {
				t.Fatalf("Resolve() error = %v, want one matching path.ErrBadPattern", err)
			}
			if !strings.Contains(err.Error(), "[") {
				t.Errorf("Resolve() error %q does not name the offending pattern %q", err, "[")
			}
		})
	})

	t.Run("ScanSpec fields merge individually", func(t *testing.T) {
		// corp sets every ScanSpec field, so any field that comes back
		// unchanged below was inherited rather than reset.
		corp := Profile{
			Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
			Scan: ScanSpec{
				Paths:    []string{"/var/log", "/opt/app/logs"},
				MaxDepth: 5,
				Include:  []string{"*.log", "*.out"},
				Exclude:  []string{"*.gz"},
			},
		}

		runResolveCases(t, []resolveCase{
			{
				name: "a rule overriding paths keeps the profile's include",
				cfg: &Config{
					Profiles: map[string]Profile{"corp": corp},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp", Scan: ScanSpec{Paths: []string{"/tmp/debug"}}},
					},
				},
				host: "web-01.prod.internal",
				want: ResolvedHost{
					Name:        "web-01.prod.internal",
					ProfileName: "corp",
					Profile: Profile{
						Exec: corp.Exec,
						Scan: ScanSpec{
							Paths:    []string{"/tmp/debug"},
							MaxDepth: 5,
							Include:  []string{"*.log", "*.out"},
							Exclude:  []string{"*.gz"},
						},
					},
				},
			},
			{
				name: "an explicitly empty paths list overrides rather than inherits",
				cfg: &Config{
					Profiles: map[string]Profile{"corp": corp},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp", Scan: ScanSpec{Paths: []string{}}},
					},
				},
				host: "web-01.prod.internal",
				want: ResolvedHost{
					Name:        "web-01.prod.internal",
					ProfileName: "corp",
					Profile: Profile{
						Exec: corp.Exec,
						Scan: ScanSpec{
							Paths:    []string{},
							MaxDepth: 5,
							Include:  []string{"*.log", "*.out"},
							Exclude:  []string{"*.gz"},
						},
					},
				},
			},
			{
				name: "a zero max_depth inherits the level below",
				cfg: &Config{
					Profiles: map[string]Profile{"corp": corp},
					Hosts: map[string]HostEntry{
						"jenkins-01": {
							Profile: "corp",
							Scan:    ScanSpec{Paths: []string{"/var/lib/jenkins/logs"}, MaxDepth: 0},
						},
					},
				},
				host: "jenkins-01",
				want: ResolvedHost{
					Name:        "jenkins-01",
					ProfileName: "corp",
					Profile: Profile{
						Exec: corp.Exec,
						Scan: ScanSpec{
							Paths:    []string{"/var/lib/jenkins/logs"},
							MaxDepth: 5,
							Include:  []string{"*.log", "*.out"},
							Exclude:  []string{"*.gz"},
						},
					},
				},
			},
			{
				name: "an empty --path flag overrides every level below it",
				cfg: &Config{
					Profiles: map[string]Profile{"corp": corp},
					Hosts: map[string]HostEntry{
						"jenkins-01": {
							Profile: "corp",
							Scan:    ScanSpec{Paths: []string{"/var/lib/jenkins/logs"}},
						},
					},
				},
				host: "jenkins-01",
				ov:   Overrides{Paths: ptr([]string{})},
				want: ResolvedHost{
					Name:        "jenkins-01",
					ProfileName: "corp",
					Profile: Profile{
						Exec: corp.Exec,
						Scan: ScanSpec{
							Paths:    []string{},
							MaxDepth: 5,
							Include:  []string{"*.log", "*.out"},
							Exclude:  []string{"*.gz"},
						},
					},
				},
			},
		})
	})

	t.Run("a host matching nothing resolves to the default profile", func(t *testing.T) {
		runResolveCases(t, []resolveCase{
			{
				name: "an empty config resolves to the built-in defaults",
				cfg:  &Config{},
				host: "laptop",
				want: ResolvedHost{
					Name:        "laptop",
					ProfileName: DefaultProfileName,
					Profile:     DefaultProfile,
				},
			},
			{
				name: "a nil config resolves to the built-in defaults",
				cfg:  nil,
				host: "laptop",
				want: ResolvedHost{
					Name:        "laptop",
					ProfileName: DefaultProfileName,
					Profile:     DefaultProfile,
				},
			},
			{
				name: "rules and host entries for other hosts are ignored",
				cfg: &Config{
					Profiles: map[string]Profile{
						"corp": {Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"}},
					},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp"},
					},
					Hosts: map[string]HostEntry{
						"jenkins-01": {Profile: "corp", Label: "CI primary"},
					},
				},
				host: "laptop",
				want: ResolvedHost{
					Name:        "laptop",
					ProfileName: DefaultProfileName,
					Profile:     DefaultProfile,
				},
			},
			{
				name: "a defined default profile is applied when nothing matches",
				cfg: &Config{
					Profiles: map[string]Profile{
						"default": {
							Exec: []string{"ssh", "-q", "{host}", "--", "{cmd}"},
							Scan: ScanSpec{Paths: []string{"/srv/logs"}},
						},
						"corp": {Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"}},
					},
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "corp"},
					},
				},
				host: "laptop",
				want: ResolvedHost{
					Name:        "laptop",
					ProfileName: DefaultProfileName,
					Profile: Profile{
						Exec: []string{"ssh", "-q", "{host}", "--", "{cmd}"},
						Scan: ScanSpec{
							Paths:    []string{"/srv/logs"},
							MaxDepth: DefaultProfile.Scan.MaxDepth,
							Include:  DefaultProfile.Scan.Include,
							Exclude:  DefaultProfile.Scan.Exclude,
						},
					},
				},
			},
		})
	})

	t.Run("a host entry naming an undefined profile is an error", func(t *testing.T) {
		defined := map[string]Profile{
			"corp": {Exec: []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"}},
		}

		cases := []struct {
			name    string
			cfg     *Config
			host    string
			ov      Overrides
			profile string
		}{
			{
				name: "the host entry names a profile that does not exist",
				cfg: &Config{
					Profiles: defined,
					Hosts: map[string]HostEntry{
						"jenkins-01": {Profile: "ghost", Label: "CI primary"},
					},
				},
				host:    "jenkins-01",
				profile: "ghost",
			},
			{
				name: "the matching rule names a profile that does not exist",
				cfg: &Config{
					Profiles: defined,
					Matches: []MatchRule{
						{Host: "*.prod.internal", Profile: "ghost"},
					},
				},
				host:    "web-01.prod.internal",
				profile: "ghost",
			},
			{
				name: "the --profile flag names a profile that does not exist",
				cfg: &Config{
					Profiles: defined,
					Hosts: map[string]HostEntry{
						"jenkins-01": {Profile: "corp"},
					},
				},
				host:    "jenkins-01",
				ov:      Overrides{Profile: ptr("ghost")},
				profile: "ghost",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := Resolve(tc.cfg, tc.host, tc.ov)
				if !errors.Is(err, ErrUnknownProfile) {
					t.Fatalf("Resolve(cfg, %q, %+v) error = %v, want one matching ErrUnknownProfile", tc.host, tc.ov, err)
				}
				for _, want := range []string{tc.host, tc.profile} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Resolve() error %q does not mention %q", err, want)
					}
				}
				if diff := cmp.Diff(ResolvedHost{}, got); diff != "" {
					t.Errorf("Resolve() returned a non-zero ResolvedHost alongside its error (-want +got):\n%s", diff)
				}
			})
		}
	})
}
