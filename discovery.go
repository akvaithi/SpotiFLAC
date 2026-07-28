package main

import "github.com/afkarxyz/SpotiFLAC/backend"

// GetRelatedArtists returns Spotify's "fans also like" for an artist id, URI or
// open.spotify.com URL. Used by clients that browse the catalog rather than
// downloading a URL that was pasted in.
func (a *App) GetRelatedArtists(artistID string) ([]backend.RelatedArtist, error) {
	return backend.GetRelatedArtists(artistID)
}

// GetArtistMembers resolves a band's lineup from MusicBrainz.
func (a *App) GetArtistMembers(artistName string) (backend.ArtistMembersResult, error) {
	return backend.GetArtistMembers(artistName)
}
