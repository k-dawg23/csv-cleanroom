package jobs

import (
	"csv-cleanroom/internal/csvdata"
	"errors"
	"testing"
	"time"
)

func TestExpiryCleanupAndSuccessfulRefresh(t *testing.T) {
	now := time.Now()
	m := New(func() time.Time { return now })
	defer m.Close()
	l, e := m.Acquire(true, "")
	if e != nil {
		t.Fatal(e)
	}
	tok, e := l.CommitUpload(&csvdata.Dataset{Header: []string{"x"}}, "x.csv")
	if e != nil {
		t.Fatal(e)
	}
	l.End(true)
	now = now.Add(Lifetime - time.Nanosecond)
	l, e = m.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Nanosecond)
	l.End(true)
	now = now.Add(Lifetime - time.Nanosecond)
	l, e = m.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	l.End(false)
	now = now.Add(time.Nanosecond)
	if _, e = m.Acquire(false, tok); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil {
		t.Fatal("expired data retained")
	}
}
func TestClosedLeaseCannotPublish(t *testing.T) {
	m := New(nil)
	l, e := m.Acquire(true, "")
	if e != nil {
		t.Fatal(e)
	}
	m.Close()
	if _, e = l.CommitUpload(&csvdata.Dataset{Header: []string{"a"}}, "x"); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	l.End(false)
}

func TestSnapshotPublicationGenerations(t *testing.T) {
	m := New(nil)
	defer m.Close()
	l, e := m.Acquire(true, "")
	if e != nil {
		t.Fatal(e)
	}
	original := &csvdata.Dataset{Header: []string{"name"}, Rows: [][]string{{" x "}}}
	token, e := l.CommitUpload(original, "x.csv")
	if e != nil {
		t.Fatal(e)
	}
	l.End(true)
	l, e = m.Acquire(false, token)
	if e != nil {
		t.Fatal(e)
	}
	old, oldReport, e := csvdata.Process(l.Job.Original, csvdata.Options{})
	if e != nil {
		t.Fatal(e)
	}
	first := &Snapshot{Data: old, Preview: old.Preview(), Report: oldReport}
	if e = l.Publish(first); e != nil {
		t.Fatal(e)
	}
	l.End(true)
	l, e = m.Acquire(false, token)
	if e != nil {
		t.Fatal(e)
	}
	ready, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		candidate, report, err := csvdata.Process(l.Job.Original, csvdata.Options{TrimWhitespace: true})
		if err != nil {
			done <- err
			return
		}
		close(ready)
		<-proceed
		done <- l.Publish(&Snapshot{Data: candidate, Preview: candidate.Preview(), Report: report})
	}()
	<-ready
	if l.Job.Snapshot != first || l.Job.Snapshot.Data.Rows[0][0] != " x " {
		t.Fatal("candidate visible before publication")
	}
	if _, e = m.Acquire(false, token); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	close(proceed)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	s := l.Job.Snapshot
	if s == first || s.Data.Rows[0][0] != "x" || s.Preview.Rows[0][0] != "x" || s.Report.CellsTrimmed.Count != 1 || original.Rows[0][0] != " x " {
		t.Fatal("inconsistent published generation")
	}
	l.End(true)
	l, e = m.Acquire(false, token)
	if e != nil {
		t.Fatal(e)
	}
	if l.Job.Snapshot != s {
		t.Fatal("published generation lost")
	}
	l.End(false)
}
