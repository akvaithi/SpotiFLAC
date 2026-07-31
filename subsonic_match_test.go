package main

import "testing"

func TestSongMatchesSeparators(t *testing.T) {
	cases := []struct {
		navTitle, navArtist, wantTitle, wantArtist string
		expect                                     bool
	}{
		{"London Thumakda", "Labh Janjua • Sonu Kakkar • Neha Kakkar", "London Thumakda", "Labh Janjua", true},
		{"London Thumakda", "Labh Janjua, Sonu Kakkar", "London Thumakda", "Labh Janjua", true},
		{"Nightcall", "Kavinsky", "Nightcall", "Kavinsky", true},
		{"Nightcall", "Kavinsky", "Nightcall", "", true},
		{"Nightcall", "Kavinsky", "Midnight City", "Kavinsky", false},
		{"Nightcall", "Kavinsky", "Nightcall", "Daft Punk", false},
		{"", "x", "", "x", false},
	}
	for _, c := range cases {
		if got := songMatches(c.navTitle, c.navArtist, c.wantTitle, c.wantArtist); got != c.expect {
			t.Errorf("songMatches(%q,%q,%q,%q) = %v, want %v", c.navTitle, c.navArtist, c.wantTitle, c.wantArtist, got, c.expect)
		}
	}
}
