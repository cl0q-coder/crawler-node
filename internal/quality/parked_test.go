package quality

import "testing"

func TestDetectParked(t *testing.T) {
	cases := []struct {
		name   string
		page   Page
		parked bool
	}{
		{
			name: "aftermarket lander",
			page: Page{Domain: "example.com", Title: "example.com for sale",
				Text:      "This domain is for sale. Buy this domain today. Make an offer below.",
				WordCount: 120},
			parked: true,
		},
		{
			name: "registrar parking template",
			page: Page{Domain: "example.net", Title: "example.net",
				Text:      "This domain is parked free, courtesy of the domain owner. Related Searches: cheap flights, insurance quotes.",
				WordCount: 30},
			parked: true,
		},
		{
			name: "single ad frame",
			page: Page{Domain: "example.org", Title: "Welcome",
				Text: "", WordCount: 0, SingleAdFrame: true, OutboundAdLinks: 5},
			parked: true,
		},
		{
			name:   "tiny text plus ad links",
			page:   Page{Domain: "example.io", Title: "example.io", Text: "This domain is parked.", WordCount: 4, OutboundAdLinks: 4},
			parked: true,
		},
		{
			// SafeSearch lesson: legit small sites must NOT trip the detector.
			name: "legit blog homepage",
			page: Page{Domain: "janesblog.com", Title: "Jane's notes on gardening",
				Text:      "Welcome to my garden journal. This week the tomatoes finally ripened and I made sauce from scratch.",
				WordCount: 80},
			parked: false,
		},
		{
			// "for sale" in prose about something else must not trip it.
			name: "legit shop page mentioning a sale",
			page: Page{Domain: "cornerstore.com", Title: "Corner Store — bikes",
				Text:      "Our winter sale is on: all bikes 20% off. Make an offer in store only, this week.",
				WordCount: 200},
			parked: false,
		},
		{
			// "make an offer" alone (0.20) stays under threshold.
			name:   "classifieds-style legit page",
			page:   Page{Domain: "usedcars.example", Title: "Used cars", Text: "Make an offer on any vehicle in our lot this weekend.", WordCount: 150},
			parked: false,
		},
		{
			name:   "empty page, no signals",
			page:   Page{Domain: "empty.example", Title: "", Text: "", WordCount: 0},
			parked: false,
		},
		{
			// "Parked" as a verb in normal prose.
			name: "legit article with parked in prose",
			page: Page{Domain: "historyblog.com", Title: "The history of parking meters",
				Text:      "The first parking meters appeared in Oklahoma City. Cars were parked by the owner along the main street.",
				WordCount: 120},
			parked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := DetectParked(tc.page)
			if v.Parked != tc.parked {
				t.Errorf("Parked=%v (score %.2f, reasons %v), want %v",
					v.Parked, v.Score, v.Reasons, tc.parked)
			}
			if v.Score < 0 || v.Score > 1 {
				t.Errorf("score %.2f out of [0,1]", v.Score)
			}
		})
	}
}

func TestDetectParkedReasonsAuditable(t *testing.T) {
	v := DetectParked(Page{Domain: "example.com", Title: "example.com for sale",
		Text: "Buy this domain.", WordCount: 50})
	if !v.Parked {
		t.Fatal("expected parked")
	}
	if len(v.Reasons) == 0 {
		t.Fatal("expected auditable reasons")
	}
	for _, r := range v.Reasons {
		if r == "" {
			t.Fatal("empty reason string")
		}
	}
	t.Logf("score=%.2f reasons=%v", v.Score, v.Reasons)
}
