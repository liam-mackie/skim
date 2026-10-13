package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDeletedCommentJSONContainsOnlyRevisionIdentity(t *testing.T) {
	comment := Comment{ID: "comment", Revision: 4, Status: "deleted", Body: "must not serialize", Path: "private.go", StartLine: 2, EndLine: 3, Side: "RIGHT", Source: "verification", ThreadID: "private-thread", HeadSHA: "old-head", CreatedAt: "private-time", Stale: true}
	encoded, err := json.Marshal(comment)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": "comment", "revision": float64(4), "status": "deleted"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("deleted JSON retained content: %s", encoded)
	}
	comment.Status = "suggested"
	encoded, err = json.Marshal(comment)
	if err != nil {
		t.Fatal(err)
	}
	var live Comment
	if err := json.Unmarshal(encoded, &live); err != nil {
		t.Fatal(err)
	}
	if live != comment {
		t.Fatalf("live JSON lost fields: %+v", live)
	}
}
