package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/render"
)

const (
	botID  = int64(7)
	iid    = 3
	headSH = "abc123"
)

var target = Target{Kind: "plan", Scope: "prod"}

func markedBody(text string) string {
	return render.Marker(target.Kind, target.Scope) + "\n" + text
}

// fakeStore is an in-memory NoteStore with hooks to force interleavings.
type fakeStore struct {
	mu     sync.Mutex
	notes  []gitlab.Note
	nextID int64
	sha    string
	writes int

	barrierNeed     int           // first barrierNeed ListNotes calls rendezvous after snapshotting
	barrierArrived  int           // callers that have snapshotted
	barrierRelease  chan struct{} // closed once barrierNeed callers arrived
	barrierTimedOut bool          // a caller gave up waiting (test must fail)

	vanishOnUpdateID    int64        // delete this note just before its first update, then 404
	deleteAfterUpdateID int64        // delete this note right after a successful update
	injectOnCreate      *gitlab.Note // add this note (a concurrent run's) right after our create
	purgeAfterWrite     bool         // every create/update is followed by deleting all notes
}

const barrierWait = 5 * time.Second

func newFake(notes ...gitlab.Note) *fakeStore {
	f := &fakeStore{sha: headSH, notes: notes, nextID: 100}
	return f
}

func (f *fakeStore) CurrentUser(context.Context) (gitlab.User, error) {
	return gitlab.User{ID: botID, Username: "bot"}, nil
}

func (f *fakeStore) MergeRequest(context.Context, int) (gitlab.MergeRequest, error) {
	return gitlab.MergeRequest{IID: iid, SHA: f.sha}, nil
}

func (f *fakeStore) ListNotes(context.Context, int) ([]gitlab.Note, error) {
	f.mu.Lock()
	snap := append([]gitlab.Note(nil), f.notes...)
	wait := f.barrierArrived < f.barrierNeed
	if wait {
		f.barrierArrived++
		if f.barrierArrived == f.barrierNeed {
			close(f.barrierRelease)
		}
	}
	release := f.barrierRelease
	f.mu.Unlock()
	if wait {
		select {
		case <-release:
		case <-time.After(barrierWait):
			f.mu.Lock()
			f.barrierTimedOut = true
			f.mu.Unlock()
		}
	}
	return snap, nil
}

func (f *fakeStore) CreateNote(_ context.Context, _ int, body string) (gitlab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.nextID++
	n := gitlab.Note{ID: f.nextID, Body: body, AuthorID: botID}
	f.notes = append(f.notes, n)
	if f.injectOnCreate != nil {
		f.notes = append(f.notes, *f.injectOnCreate)
		f.injectOnCreate = nil
	}
	if f.purgeAfterWrite {
		f.notes = nil
	}
	return n, nil
}

func (f *fakeStore) UpdateNote(_ context.Context, _ int, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if id == f.vanishOnUpdateID {
		f.vanishOnUpdateID = 0
		f.removeLocked(id)
	}
	for i := range f.notes {
		if f.notes[i].ID == id {
			f.notes[i].Body = body
			if id == f.deleteAfterUpdateID {
				f.removeLocked(id)
			}
			if f.purgeAfterWrite {
				f.notes = nil
			}
			return nil
		}
	}
	return fmt.Errorf("update %d: %w", id, gitlab.ErrNotFound)
}

func (f *fakeStore) DeleteNote(_ context.Context, _ int, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if !f.removeLocked(id) {
		return fmt.Errorf("delete %d: %w", id, gitlab.ErrNotFound)
	}
	return nil
}

func (f *fakeStore) removeLocked(id int64) bool {
	for i := range f.notes {
		if f.notes[i].ID == id {
			f.notes = append(f.notes[:i], f.notes[i+1:]...)
			return true
		}
	}
	return false
}

func (f *fakeStore) botMarked() []gitlab.Note {
	var out []gitlab.Note
	for _, n := range f.notes {
		if !n.System && n.AuthorID == botID && render.HasMarker(n.Body, target.Kind, target.Scope) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func TestUpsertCreatesWhenMissing(t *testing.T) {
	f := newFake()
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("notes = %+v", got)
	}
}

func TestUpsertUpdatesExisting(t *testing.T) {
	f := newFake(gitlab.Note{ID: 4, Body: markedBody("old"), AuthorID: botID})
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 || got[0].ID != 4 || got[0].Body != body {
		t.Fatalf("notes = %+v", got)
	}
}

func TestUpsertStaleWritesNothing(t *testing.T) {
	f := newFake()
	f.sha = "other"
	err := Upsert(context.Background(), f, iid, headSH, target, markedBody("x"))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if f.writes != 0 || len(f.notes) != 0 {
		t.Fatalf("stale run wrote: writes=%d notes=%+v", f.writes, f.notes)
	}
}

func TestUpsertIgnoresForeignMarkers(t *testing.T) {
	marker := render.Marker(target.Kind, target.Scope)
	forged := "&lt;!-- tf-review:v1:plan:prod --&gt;\ntext"
	f := newFake(
		gitlab.Note{ID: 1, Body: marker + "\nhuman", AuthorID: 99},
		gitlab.Note{ID: 2, Body: marker + "\nsystem", AuthorID: botID, System: true},
		gitlab.Note{ID: 3, Body: forged, AuthorID: botID},
		gitlab.Note{ID: 4, Body: "quoted " + marker + " inline", AuthorID: botID},
	)
	body := markedBody("mine")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	if len(f.notes) != 5 {
		t.Fatalf("foreign notes must be untouched and one created: %+v", f.notes)
	}
	for _, n := range f.notes[:4] {
		if n.Body == body {
			t.Fatalf("foreign note %d was overwritten", n.ID)
		}
	}
	if got := f.botMarked(); len(got) != 1 || got[0].Body != body {
		t.Fatalf("bot notes = %+v", got)
	}
}

func TestUpsertDedupeKeepsLowestID(t *testing.T) {
	f := newFake(
		gitlab.Note{ID: 9, Body: markedBody("dup"), AuthorID: botID},
		gitlab.Note{ID: 3, Body: markedBody("old"), AuthorID: botID},
		gitlab.Note{ID: 5, Body: markedBody("human"), AuthorID: 99},
	)
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 {
		t.Fatalf("notes = %+v", got)
	}
	if got[0].ID != 3 || got[0].Body != body {
		t.Fatalf("kept = %+v", got[0])
	}
	if len(f.notes) != 2 {
		t.Fatalf("human note must survive: %+v", f.notes)
	}
}

func TestUpsertRecreatesWhenNoteVanishes(t *testing.T) {
	f := newFake(gitlab.Note{ID: 4, Body: markedBody("old"), AuthorID: botID})
	f.vanishOnUpdateID = 4
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 || got[0].Body != body || got[0].ID == 4 {
		t.Fatalf("notes = %+v", got)
	}
}

func TestUpsertRedoesWhenKeptNoteVanishesDuringConverge(t *testing.T) {
	// A concurrent run's lower-ID note (50) shows up after our create (101),
	// then is deleted before we can update it: we must not delete our own note.
	f := newFake()
	f.injectOnCreate = &gitlab.Note{ID: 50, Body: markedBody("other"), AuthorID: botID}
	f.vanishOnUpdateID = 50
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("notes = %+v", got)
	}
}

func TestUpsertRedoesWhenNoteVanishesAfterWrite(t *testing.T) {
	f := newFake(gitlab.Note{ID: 4, Body: markedBody("old"), AuthorID: botID})
	f.deleteAfterUpdateID = 4
	body := markedBody("new")
	if err := Upsert(context.Background(), f, iid, headSH, target, body); err != nil {
		t.Fatal(err)
	}
	got := f.botMarked()
	if len(got) != 1 || got[0].Body != body || got[0].ID == 4 {
		t.Fatalf("notes = %+v", got)
	}
}

func TestUpsertGivesUpWhenNoteKeepsVanishing(t *testing.T) {
	f := newFake()
	f.purgeAfterWrite = true
	err := Upsert(context.Background(), f, iid, headSH, target, markedBody("x"))
	if err == nil {
		t.Fatal("want error when the note never sticks")
	}
	if f.writes > 2*maxAttempts {
		t.Fatalf("unbounded retries: %d writes", f.writes)
	}
}

func TestUpsertConcurrentRunsConverge(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"same body", markedBody("v"), markedBody("v")},
		{"different bodies", markedBody("A"), markedBody("B")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.barrierNeed = 2 // both first lists happen before either create
			f.barrierRelease = make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i, b := range []string{tc.a, tc.b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = Upsert(context.Background(), f, iid, headSH, target, b)
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if f.barrierTimedOut {
				t.Fatal("barrier never released: interleaving was not forced")
			}
			got := f.botMarked()
			if len(got) != 1 {
				t.Fatalf("want exactly one bot note, got %+v", got)
			}
			if got[0].Body != tc.a && got[0].Body != tc.b {
				t.Fatalf("unexpected body %q", got[0].Body)
			}
		})
	}
}

func TestUpsertRejectsBodyWithoutMarker(t *testing.T) {
	f := newFake()
	if err := Upsert(context.Background(), f, iid, headSH, target, "no marker"); err == nil {
		t.Fatal("body without marker must be rejected")
	}
	if f.writes != 0 {
		t.Fatal("nothing may be written")
	}
}
