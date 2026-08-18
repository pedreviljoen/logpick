package transport

import (
	"testing"

	"github.com/pedreviljoen/logpick/internal/config"
)

// TestNewSelectsBackendByPersistent covers New's one job: mapping
// profile.Persistent onto a concrete backend. A persistent profile must get
// a *Persistent (the interactive-shell backend for wrappers like ec2-ssh
// that take no remote command), and a non-persistent one must get a
// *Command (one process per command). Before New existed both call sites
// hard-coded NewCommand and silently ignored the flag, so the whole point
// of this test is that the flag is honoured.
func TestNewSelectsBackendByPersistent(t *testing.T) {
	t.Run("persistent profile gets a Persistent backend", func(t *testing.T) {
		p := config.Profile{
			Exec:       []string{"ec2-ssh", "{host}"},
			Persistent: true,
		}
		tp := New("devStack", p)
		defer func() { _ = tp.Close() }()

		if _, ok := tp.(*Persistent); !ok {
			t.Fatalf("New returned %T, want *Persistent", tp)
		}
	})

	t.Run("non-persistent profile gets a Command backend", func(t *testing.T) {
		p := config.Profile{Exec: []string{"ssh", "{host}", "--", "{cmd}"}}
		tp := New("devStack", p)
		defer func() { _ = tp.Close() }()

		if _, ok := tp.(*Command); !ok {
			t.Fatalf("New returned %T, want *Command", tp)
		}
	})
}
