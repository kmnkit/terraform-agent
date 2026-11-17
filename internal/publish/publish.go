// Package publish upserts the bot's MR note, keeping exactly one per marker.
package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/render"
)

// maxAttempts bounds write+converge redo rounds when notes vanish mid-run.
const maxAttempts = 2

// ErrStale means the MR head moved past the commit the review was made for.
var ErrStale = errors.New("publish: merge request head changed")

// NoteStore is the subset of the GitLab client that Upsert needs.
type NoteStore interface {
	CurrentUser(ctx context.Context) (gitlab.User, error)
	MergeRequest(ctx context.Context, iid int) (gitlab.MergeRequest, error)
	ListNotes(ctx context.Context, iid int) ([]gitlab.Note, error)
	CreateNote(ctx context.Context, iid int, body string) (gitlab.Note, error)
	UpdateNote(ctx context.Context, iid int, noteID int64, body string) error
	DeleteNote(ctx context.Context, iid int, noteID int64) error
}

// Target identifies which bot note to upsert: the marker's kind and scope.
// A struct (rather than a pre-rendered marker string) lets Upsert call
// render.HasMarker, the only sanctioned marker matcher.
type Target struct {
	Kind  string
	Scope string
}

// Upsert writes body (which must contain the target marker) as the bot's single
// note for target on the MR. It writes nothing if the MR head is not wantSHA.
// Concurrent runs converge: after writing, every run re-lists, keeps the
// lowest-ID bot marker note with body, and deletes the rest.
func Upsert(ctx context.Context, store NoteStore, mrIID int, wantSHA string, target Target, body string) error {
	if !render.HasMarker(body, target.Kind, target.Scope) {
		return errors.New("publish: body lacks the target marker")
	}
	mr, err := store.MergeRequest(ctx, mrIID)
	if err != nil {
		return fmt.Errorf("publish: get merge request: %w", err)
	}
	if mr.SHA != wantSHA {
		return ErrStale
	}
	bot, err := store.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("publish: get current user: %w", err)
	}
	for range maxAttempts {
		err := writeAndConverge(ctx, store, mrIID, bot.ID, target, body)
		if !errors.Is(err, errVanished) {
			return err
		}
	}
	return fmt.Errorf("publish: note kept vanishing after %d attempts", maxAttempts)
}

func writeAndConverge(ctx context.Context, store NoteStore, iid int, botID int64, target Target, body string) error {
	mine, err := botNotes(ctx, store, iid, botID, target)
	if err != nil {
		return err
	}
	written, err := write(ctx, store, iid, mine, body)
	if err != nil {
		return err
	}
	return converge(ctx, store, iid, botID, target, body, written)
}

// errVanished means a note we depend on was deleted mid-run; redo the write.
var errVanished = errors.New("publish: note vanished mid-run")

// write updates the lowest-ID note, or creates one when none exists or the
// note vanished mid-run. It returns the ID of the note that now holds body.
func write(ctx context.Context, store NoteStore, iid int, mine []gitlab.Note, body string) (int64, error) {
	if len(mine) > 0 {
		err := store.UpdateNote(ctx, iid, mine[0].ID, body)
		if err == nil {
			return mine[0].ID, nil
		}
		if !errors.Is(err, gitlab.ErrNotFound) {
			return 0, fmt.Errorf("publish: update note: %w", err)
		}
	}
	n, err := store.CreateNote(ctx, iid, body)
	if err != nil {
		return 0, fmt.Errorf("publish: create note: %w", err)
	}
	return n.ID, nil
}

// converge re-lists, makes the lowest-ID bot marker note carry body, and
// deletes every other bot marker note. Only a DELETE 404 is tolerated (another
// run may have deleted the same note); an empty list or an update 404 returns
// errVanished so the caller redoes the write.
func converge(ctx context.Context, store NoteStore, iid int, botID int64, target Target, body string, written int64) error {
	mine, err := botNotes(ctx, store, iid, botID, target)
	if err != nil {
		return err
	}
	if len(mine) == 0 {
		return errVanished
	}
	keep := mine[0].ID
	if keep != written {
		// No duplicate is deleted before the kept note holds body, so a 404
		// here must not fall through to deleting the note we wrote.
		err := store.UpdateNote(ctx, iid, keep, body)
		if errors.Is(err, gitlab.ErrNotFound) {
			return errVanished
		}
		if err != nil {
			return fmt.Errorf("publish: update kept note: %w", err)
		}
	}
	for _, n := range mine[1:] {
		err := store.DeleteNote(ctx, iid, n.ID)
		if err != nil && !errors.Is(err, gitlab.ErrNotFound) {
			return fmt.Errorf("publish: delete duplicate note: %w", err)
		}
	}
	return nil
}

// botNotes returns the bot's own non-system notes carrying the target marker,
// ordered by ID ascending.
func botNotes(ctx context.Context, store NoteStore, iid int, botID int64, target Target) ([]gitlab.Note, error) {
	all, err := store.ListNotes(ctx, iid)
	if err != nil {
		return nil, fmt.Errorf("publish: list notes: %w", err)
	}
	var out []gitlab.Note
	for _, n := range all {
		if !n.System && n.AuthorID == botID && render.HasMarker(n.Body, target.Kind, target.Scope) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b gitlab.Note) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}
