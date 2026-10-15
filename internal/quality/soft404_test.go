package quality

import "testing"

func TestDetectSoft404(t *testing.T) {
	cases := []struct {
		name    string
		page    Page
		soft404 bool
	}{
		{
			name: "classic soft-404 template",
			page: Page{Domain: "example.com", Title: "404 - Page Not Found",
				Text:      "Oops! That page can't be found. The page you are looking for might have been removed or had its name changed.",
				WordCount: 60},
			soft404: true,
		},
		{
			name: "empty 200 with bare title",
			page: Page{Domain: "dead.example", Title: "dead.example",
				Text: "", WordCount: 0},
			soft404: true, // 0.55 empty + 0.20 thin = 0.75
		},
		{
			name: "empty 200 with 404 title",
			page: Page{Domain: "gone.example", Title: "Page Not Found",
				Text: "", WordCount: 0},
			soft404: true, // 0.55 empty + 0.40 title phrase = 0.95
		},
		{
			name: "coming-soon placeholder",
			page: Page{Domain: "newsite.example", Title: "newsite.example",
				Text:      "This site is coming soon. We are under construction — check back later.",
				WordCount: 14},
			soft404: true, // 0.45 template + 0.075+0.075 body halves + 0.20 thin = 0.80
		},
		{
			name: "default web server page",
			page: Page{Domain: "srv.example", Title: "Default Web Site Page",
				Text:      "If you are seeing this page, the web server is installed and working.",
				WordCount: 14},
			soft404: true,
		},
		{
			name: "bare placeholder with empty text",
			page: Page{Domain: "blank.example", Title: "",
				Text: "Content goes here.", WordCount: 3},
			soft404: true, // 0.45 template + 0.10 body half + 0.20 thin = 0.75
		},
		{
			// SafeSearch lesson: a JS-rendered legit page has empty raw text
			// but a REAL title. Empty alone (0.55) must not condemn it.
			name: "js-app shell, empty text, real title",
			page: Page{Domain: "app.example", Title: "Acme Dashboard",
				Text: "", WordCount: 0},
			soft404: false,
		},
		{
			// Articles ABOUT 404 handling mention the phrases in body prose
			// with a normal title. Body matches count half weight.
			name: "legit article about 404 handling",
			page: Page{Domain: "devblog.example", Title: "How we handle 404s in our router",
				Text:      "When a page cannot be found we return a friendly page not found screen. Sorry, we couldn't find that page is the copy we use.",
				WordCount: 400},
			soft404: false,
		},
		{
			// "Coming soon" in legit prose on a real, titled page.
			name: "legit store announcing coming soon",
			page: Page{Domain: "cornerstore.com", Title: "Corner Store — bikes",
				Text:      "Our new downtown location is coming soon! We sell road, gravel and city bikes with free servicing.",
				WordCount: 120},
			soft404: false,
		},
		{
			// Short but real: thin corroboration needs a bare/empty title.
			name: "legit contact stub",
			page: Page{Domain: "plumber.example", Title: "Contact",
				Text:      "Call 555-0100. 12 Main St, open Mon–Fri 8–5.",
				WordCount: 12},
			soft404: false,
		},
		{
			name: "legit blog homepage",
			page: Page{Domain: "janesblog.com", Title: "Jane's notes on gardening",
				Text:      "Welcome to my garden journal. This week the tomatoes finally ripened and I made sauce from scratch.",
				WordCount: 80},
			soft404: false,
		},
		{
			// K1's job, not K2's: parked pages should not necessarily trip
			// the soft-404 detector (no double-flagging noise).
			name: "parked page does not trip soft404",
			page: Page{Domain: "parked.example", Title: "parked.example for sale",
				Text:      "This domain is for sale. Buy this domain today. Make an offer below.",
				WordCount: 120},
			soft404: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := DetectSoft404(tc.page)
			if v.Soft404 != tc.soft404 {
				t.Errorf("Soft404=%v (score %.2f, reasons %v), want %v",
					v.Soft404, v.Score, v.Reasons, tc.soft404)
			}
			if v.Score < 0 || v.Score > 1 {
				t.Errorf("score %.2f out of [0,1]", v.Score)
			}
		})
	}
}

func TestDetectSoft404ReasonsAuditable(t *testing.T) {
	v := DetectSoft404(Page{Domain: "example.com", Title: "404 - Page Not Found",
		Text: "The page you are looking for is gone.", WordCount: 20})
	if !v.Soft404 {
		t.Fatal("expected soft404")
	}
	if len(v.Reasons) == 0 {
		t.Fatal("expected auditable reasons")
	}
	for _, r := range v.Reasons {
		if r == "" {
			t.Fatal("empty reason string")
		}
	}
}
