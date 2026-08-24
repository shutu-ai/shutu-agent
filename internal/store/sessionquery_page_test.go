package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func TestSearchSessionsPageBoundsAndCWD(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		log := session.New()
		_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("pageable marker"))
		if err := st.AppendEvents(ctx, id, log.Events()); err != nil {
			t.Fatal(err)
		}
	}
	page, more, err := st.SearchSessionsPage(ctx, "pageable", 0, 2)
	if err != nil || len(page) != 2 || !more {
		t.Fatalf("first page len=%d more=%v err=%v", len(page), more, err)
	}
	next, more, err := st.SearchSessionsPage(ctx, "pageable", 2, 2)
	if err != nil || len(next) != 1 || more {
		t.Fatalf("second page len=%d more=%v err=%v", len(next), more, err)
	}
	meta, err := st.GetSessionMeta(ctx, "s1")
	if err != nil || meta.CWD == "" {
		t.Fatalf("session cwd=%q err=%v", meta.CWD, err)
	}
}
