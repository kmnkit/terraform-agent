package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

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

	listBarrier      *sync.WaitGroup // first barrierLeft ListNotes calls rendezvous after snapshotting
	barrierLeft      int
	vanishOnUpdateID int64 // delete this note just before its first update
}

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
	var barrier *sync.WaitGroup
	if f.barrierLeft > 0 {
		f.barrierLeft--
		barrier = f.listBarrier
	}
	f.mu.Unlock()
	if barrier != nil {
		barrier.Done()
		barrier.Wait()
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

func TestUpsertConcurrentRunsConverge(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"same body", markedBody("v"), markedBody("v")},
		{"different bodies", markedBody("A"), markedBody("B")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.listBarrier = &sync.WaitGroup{}
			f.listBarrier.Add(2) // both first lists happen before either create
			f.barrierLeft = 2
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
