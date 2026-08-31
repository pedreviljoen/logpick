package library_test

// Test plan (AGENTS.md section 5 and T16's Contract, copied verbatim before
// any test below was written):
//
//  1. The library lists fetched entries from state and delete removes the
//     selected one.
//
// The single test below feeds a tea.Msg to Update and asserts on the
// returned Model's exported accessors (Files, Highlighted) and on the
// tea.Cmd it emitted, never on View's rendered output (DESIGN.md section
// 13). Delete is made observable through library.DeleteFunc: the test
// supplies its own, which records the path it was asked to delete, rather
// than reaching into Model's unexported state (AGENTS.md section 5).

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/library"
)

// update drives m through one Update call and type-asserts the returned
// ui.ScreenModel back to library.Model, which is what every test in this
// file needs to keep chaining calls the way real usage does.
func update(t *testing.T, m library.Model, msg tea.Msg) (library.Model, tea.Cmd) {
	t.Helper()
	screen, cmd := m.Update(msg)
	lm, ok := screen.(library.Model)
	if !ok {
		t.Fatalf("Update(%T) returned %T, want library.Model", msg, screen)
	}
	return lm, cmd
}

// filesFor builds a []ui.FetchedFile, one per local path given, each with a
// distinct Host, Remote, Bytes and At so two entries are never accidentally
// equal.
func filesFor(locals ...string) []ui.FetchedFile {
	files := make([]ui.FetchedFile, len(locals))
	for i, local := range locals {
		files[i] = ui.FetchedFile{
			Host:   "jenkins-01.prod.internal",
			Remote: "/var/log/app.log",
			Local:  local,
			Bytes:  int64(1000 + i),
			At:     time.Date(2026, 8, 17, 12, 0, i, 0, time.UTC),
		}
	}
	return files
}

// recordingDeleter is a DeleteFunc test double that records every path it
// was asked to delete and the context each call ran under, and succeeds
// unless failFor names the requested path.
type recordingDeleter struct {
	calls   []string
	ctxs    []context.Context
	failFor string
}

func (d *recordingDeleter) delete(ctx context.Context, local string) error {
	d.calls = append(d.calls, local)
	d.ctxs = append(d.ctxs, ctx)
	if local == d.failFor {
		return errors.New("delete failed")
	}
	return nil
}

func TestModel_ListsAndDeletes(t *testing.T) {
	t.Run("the library lists fetched entries from state and delete removes the selected one", func(t *testing.T) {
		deleter := &recordingDeleter{}
		m := library.New(deleter.delete)

		files := filesFor("/data/fetched/a/1/app.log", "/data/fetched/b/1/app.log", "/data/fetched/c/1/app.log")
		m, loadCmd := update(t, m, ui.LibraryLoadedMsg{Files: files})

		if diff := cmp.Diff(files, m.Files()); diff != "" {
			t.Fatalf("Files after LibraryLoadedMsg (-want +got):\n%s", diff)
		}
		if loadCmd != nil {
			t.Fatalf("Update(LibraryLoadedMsg) returned a non-nil Cmd, want nil")
		}

		highlighted, ok := m.Highlighted()
		if !ok {
			t.Fatalf("Highlighted() ok = false after loading, want true")
		}
		if diff := cmp.Diff(files[0], highlighted); diff != "" {
			t.Fatalf("Highlighted() after loading (-want +got):\n%s", diff)
		}

		m, delCmd := update(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
		if delCmd == nil {
			t.Fatalf("Update(ctrl+d) returned a nil Cmd; deleting the highlighted entry should be a returned tea.Cmd, not work done in Update")
		}
		if diff := cmp.Diff(files, m.Files()); diff != "" {
			t.Fatalf("Files changed synchronously from Update(ctrl+d) before its Cmd ran (-want +got):\n%s", diff)
		}

		msg := delCmd()
		deleted, ok := msg.(ui.LibraryDeletedMsg)
		if !ok {
			t.Fatalf("the delete Cmd reported %T, want ui.LibraryDeletedMsg", msg)
		}
		if deleted.Local != files[0].Local {
			t.Fatalf("LibraryDeletedMsg.Local = %q, want %q", deleted.Local, files[0].Local)
		}

		if diff := cmp.Diff([]string{files[0].Local}, deleter.calls); diff != "" {
			t.Fatalf("deleter was asked to delete (-want +got):\n%s", diff)
		}
		if len(deleter.ctxs) != 1 || deleter.ctxs[0] == nil {
			t.Fatalf("deleter was called with a nil context")
		}

		m, applyCmd := update(t, m, deleted)
		if applyCmd != nil {
			t.Fatalf("Update(LibraryDeletedMsg) returned a non-nil Cmd, want nil")
		}
		if diff := cmp.Diff(files[1:], m.Files()); diff != "" {
			t.Fatalf("Files after LibraryDeletedMsg (-want +got):\n%s", diff)
		}
	})
}

func TestModel_EnterOpensHighlightedFile(t *testing.T) {
	files := filesFor("/data/fetched/a/1/app.log", "/data/fetched/b/1/app.log")
	m := library.New(func(context.Context, string) error { return nil })
	m, _ = update(t, m, ui.LibraryLoadedMsg{Files: files})

	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter returned nil Cmd, want ui.FileSelectedMsg")
	}
	raw := cmd()
	got, ok := raw.(ui.FileSelectedMsg)
	if !ok {
		t.Fatalf("enter command returned %T, want ui.FileSelectedMsg", raw)
	}
	if diff := cmp.Diff(files[0], got.File); diff != "" {
		t.Fatalf("FileSelectedMsg.File mismatch (-want +got):\n%s", diff)
	}
}

func TestModel_EscapeGoesBack(t *testing.T) {
	m := library.New(func(context.Context, string) error { return nil })
	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("escape returned nil Cmd, want ui.BackMsg")
	}
	if _, ok := cmd().(ui.BackMsg); !ok {
		t.Fatalf("escape command returned %T, want ui.BackMsg", cmd())
	}
}
