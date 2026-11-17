package publish

import (
	"context"
	"errors"
	"testing"

	"github.com/terraform-agent/internal/gitlab"
)

// failingStore wraps fakeStore and fails selected operations.
type failingStore struct {
	*fakeStore
	failMR, failUser, failList, failCreate, failUpdate, failDelete bool
}

var errBoom = errors.New("boom")

func (s failingStore) MergeRequest(ctx context.Context, i int) (gitlab.MergeRequest, error) {
	if s.failMR {
		return gitlab.MergeRequest{}, errBoom
	}
	return s.fakeStore.MergeRequest(ctx, i)
}

func (s failingStore) CurrentUser(ctx context.Context) (gitlab.User, error) {
	if s.failUser {
		return gitlab.User{}, errBoom
	}
	return s.fakeStore.CurrentUser(ctx)
}

func (s failingStore) ListNotes(ctx context.Context, i int) ([]gitlab.Note, error) {
	if s.failList {
		return nil, errBoom
	}
	return s.fakeStore.ListNotes(ctx, i)
}

func (s failingStore) CreateNote(ctx context.Context, i int, b string) (gitlab.Note, error) {
	if s.failCreate {
		return gitlab.Note{}, errBoom
	}
	return s.fakeStore.CreateNote(ctx, i, b)
}

func (s failingStore) UpdateNote(ctx context.Context, i int, id int64, b string) error {
	if s.failUpdate {
		return errBoom
	}
	return s.fakeStore.UpdateNote(ctx, i, id, b)
}

func (s failingStore) DeleteNote(ctx context.Context, i int, id int64) error {
	if s.failDelete {
		return errBoom
	}
	return s.fakeStore.DeleteNote(ctx, i, id)
}

func TestUpsertPropagatesStoreErrors(t *testing.T) {
	dup := func() *fakeStore {
		return newFake(
			gitlab.Note{ID: 1, Body: markedBody("a"), AuthorID: botID},
			gitlab.Note{ID: 2, Body: markedBody("b"), AuthorID: botID},
		)
	}
	tests := []struct {
		name  string
		store failingStore
	}{
		{"merge request", failingStore{fakeStore: newFake(), failMR: true}},
		{"current user", failingStore{fakeStore: newFake(), failUser: true}},
		{"list", failingStore{fakeStore: newFake(), failList: true}},
		{"create", failingStore{fakeStore: newFake(), failCreate: true}},
		{"update", failingStore{fakeStore: dup(), failUpdate: true}},
		{"delete", failingStore{fakeStore: dup(), failDelete: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Upsert(context.Background(), tc.store, iid, headSH, target, markedBody("x"))
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want wrapped errBoom", err)
			}
		})
	}
}
