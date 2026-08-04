package main

import "testing"

// The bug these cover: searching the facade for a track already on disk still
// offered it for download. Every case below is a way the two sides spell the
// same recording differently — multi-singer credits and `- From "Film"` title
// suffixes, which is why it showed up first on Indian film songs.
func TestArtistsOverlapSeparatorsAndOrder(t *testing.T) {
	cases := []struct {
		a, b   string
		expect bool
	}{
		// SpotiFLAC's tagger vs Spotify, with the billing order reversed.
		{"G. V. Prakash Kumar • Shweta Mohan", "Shweta Mohan, G. V. Prakash Kumar", true},
		{"Labh Janjua • Sonu Kakkar • Neha Kakkar", "Labh Janjua", true},
		{"Anirudh Ravichander", "Anirudh Ravichander, Shilpa Rao", true},
		// One credit carries the fuller name.
		{"G. V. Prakash", "G. V. Prakash Kumar", true},
		{"A.R. Rahman & Shreya Ghoshal", "Shreya Ghoshal", true},
		// Unrelated performers must stay unrelated.
		{"Kavinsky", "Daft Punk", false},
		{"Sid Sriram", "Shreya Ghoshal", false},
		// Unknown on one side: the title match has to stand alone.
		{"Kavinsky", "", true},
	}
	for _, c := range cases {
		if got := artistsOverlap(c.a, c.b); got != c.expect {
			t.Errorf("artistsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.expect)
		}
	}
}

func TestTitleKeyCollapsesVersionQualifiers(t *testing.T) {
	same := [][2]string{
		{`Vaa Vaathi (From "Vaathi")`, `Vaa Vaathi - From "Vaathi"`},
		{`Vaa Vaathi`, `Vaa Vaathi - From "Vaathi"`},
		{"Naatu Naatu (From \"RRR\")", "Naatu Naatu"},
		{"Nightcall - Remastered", "Nightcall"},
	}
	for _, p := range same {
		if titleKey(p[0]) != titleKey(p[1]) {
			t.Errorf("titleKey(%q)=%q != titleKey(%q)=%q", p[0], titleKey(p[0]), p[1], titleKey(p[1]))
		}
	}
	if titleKey("Vaa Vaathi") == titleKey("Vaathi Coming") {
		t.Error("distinct titles collapsed")
	}
}

// Dedup groups on strictKey and then moves files to the trash, so its
// boundaries are about what must NOT merge as much as what must.
func TestStrictKeyGroupsOnlyTheSameCredit(t *testing.T) {
	same := [][2][2]string{
		// Separator and billing order differ; same two performers.
		{{"Nightcall", "Kavinsky • Angèle"}, {"Nightcall", "Angèle, Kavinsky"}},
		{{"Vaa Vaathi", "G. V. Prakash Kumar • Shweta Mohan"}, {"Vaa Vaathi", "G. V. Prakash Kumar & Shweta Mohan"}},
		{{"Intro", ""}, {"Intro", ""}},
	}
	for _, p := range same {
		if strictKey(p[0][0], p[0][1]) != strictKey(p[1][0], p[1][1]) {
			t.Errorf("should group: %v vs %v", p[0], p[1])
		}
	}

	differ := [][2][2]string{
		// The warning case: a guest credit is a different recording, and
		// merging it would offer to delete one of the two.
		{{"Nightcall", "Kavinsky"}, {"Nightcall", "Kavinsky • Angèle • Phoenix"}},
		// Versions stay apart — strictKey keeps parentheticals.
		{{"Song (Live)", "Kavinsky"}, {"Song", "Kavinsky"}},
		{{"Song (Live)", "Kavinsky"}, {"Song (Radio Edit)", "Kavinsky"}},
		// Same title, unrelated artists.
		{{"Intro", "Kendrick Lamar"}, {"Intro", "The xx"}},
		// An untagged file must not merge with a tagged one.
		{{"Intro", ""}, {"Intro", "Kendrick Lamar"}},
	}
	for _, p := range differ {
		if strictKey(p[0][0], p[0][1]) == strictKey(p[1][0], p[1][1]) {
			t.Errorf("must not group: %v vs %v (key %q)", p[0], p[1], strictKey(p[0][0], p[0][1]))
		}
	}
}

func TestMatchLibraryAcrossCreditSpellings(t *testing.T) {
	saved := library
	t.Cleanup(func() { library = saved })

	library = &libraryIndex{
		entries: map[string]*libraryEntry{
			"/music/vaa.flac": {
				Path:   "/music/vaa.flac",
				Title:  `Vaa Vaathi (From "Vaathi")`,
				Artist: "G. V. Prakash Kumar • Shweta Mohan",
			},
			// No artist tag: must not claim every track sharing its title.
			"/music/untagged.flac": {Path: "/music/untagged.flac", Title: "Intro"},
		},
		isrc:   map[string][]string{},
		titles: map[string][]string{},
	}
	library.reindexLocked()

	var app App
	cases := []struct {
		title, artist string
		expect        bool
	}{
		{`Vaa Vaathi - From "Vaathi"`, "Shweta Mohan, G. V. Prakash Kumar", true},
		{"Vaa Vaathi", "G. V. Prakash Kumar", true},
		{`Vaa Vaathi (From "Vaathi")`, "Some Other Singer", false},
		{"Intro", "Kendrick Lamar", false},
	}
	inputs := make([]LibMatchInput, len(cases))
	for i, c := range cases {
		inputs[i] = LibMatchInput{Index: i, Title: c.title, Artist: c.artist}
	}
	for _, r := range app.MatchLibrary(inputs) {
		if c := cases[r.Index]; r.InLibrary != c.expect {
			t.Errorf("MatchLibrary(%q, %q) = %v, want %v", c.title, c.artist, r.InLibrary, c.expect)
		}
	}
}
