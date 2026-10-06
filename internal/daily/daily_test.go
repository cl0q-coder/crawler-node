package daily

import (
	"testing"
	"time"
)

func TestBasic(t *testing.T) {
	tr := New()
	tr.AddDomains(10)
	tr.AddPages(50, 102400)
	tr.AddErrors(2)

	today := tr.Today()
	if today.DomainsCrawled != 10 {
		t.Errorf("DomainsCrawled = %d, want 10", today.DomainsCrawled)
	}
	if today.PagesFetched != 50 {
		t.Errorf("PagesFetched = %d, want 50", today.PagesFetched)
	}
	if today.BytesFetched != 102400 {
		t.Errorf("BytesFetched = %d, want 102400", today.BytesFetched)
	}
	if today.Errors != 2 {
		t.Errorf("Errors = %d, want 2", today.Errors)
	}
}

func TestDelta(t *testing.T) {
	tr := New()
	// Simulate yesterday
	tr.yesterday = Stats{Day: "2026-10-05", DomainsCrawled: 100, PagesFetched: 500}
	tr.AddDomains(120)
	tr.AddPages(600, 0)

	d := tr.Delta()
	if d.DomainsCrawled != 20 {
		t.Errorf("Delta Domains = %d, want 20", d.DomainsCrawled)
	}
	if d.PagesFetched != 100 {
		t.Errorf("Delta Pages = %d, want 100", d.PagesFetched)
	}
}

func TestDayFormat(t *testing.T) {
	tr := New()
	today := tr.Today()
	// Should be YYYY-MM-DD
	if len(today.Day) != 10 || today.Day[4] != '-' || today.Day[7] != '-' {
		t.Errorf("Day format = %q, want YYYY-MM-DD", today.Day)
	}
	// Should be UTC today
	want := time.Now().UTC().Format("2006-01-02")
	if today.Day != want {
		t.Errorf("Day = %q, want %q", today.Day, want)
	}
}
