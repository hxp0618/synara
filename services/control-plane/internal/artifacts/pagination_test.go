package artifacts

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestArtifactListCursorIsOpaqueAndSessionBound(t *testing.T) {
	cursor := artifactListCursor{
		Version: 1, TenantID: uuid.New(), SessionID: uuid.New(), CreatedAt: time.Now().UTC(), ID: uuid.New(),
	}
	encoded := encodeArtifactListCursor(cursor)
	decoded, err := decodeArtifactListCursor(encoded)
	if err != nil || decoded != cursor {
		t.Fatalf("decoded Artifact cursor = %#v err=%v", decoded, err)
	}
	if _, err := decodeArtifactListCursor("not-a-cursor"); err == nil {
		t.Fatal("invalid Artifact cursor was accepted")
	}
}
