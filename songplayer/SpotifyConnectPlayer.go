package songplayer

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zmb3/spotify"
	"golang.org/x/oauth2"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
)

const DEFAULT_AUTHORISE_PORT = 5678
const DEFAULT_AUTHORISE_URL = "http://musicbot:5678/authorise/"

var ErrNotAuthorised = errors.New("[SpotifyConnectPlayer] Client has not been authorised yet")

type SpotifyConnectPlayer struct {
	sessionKey    string
	tokenFilePath string
	client        *spotify.Client
	user          *spotify.PrivateUser
	auth          *spotify.Authenticator
	authListeners []func()
}

func NewSpotifyConnectPlayer(spotifyClientID, spotifyClientSecret, tokenFilePath, authoriseRedirectURL string, authoriseHTTPPort int) (p *SpotifyConnectPlayer, authURL string, err error) {
	if authoriseRedirectURL == "" {
		authoriseRedirectURL = DEFAULT_AUTHORISE_URL
	}
	if authoriseHTTPPort <= 0 {
		authoriseHTTPPort = DEFAULT_AUTHORISE_PORT
	}

	auth := spotify.NewAuthenticator(authoriseRedirectURL, spotify.ScopeUserReadCurrentlyPlaying, spotify.ScopeUserReadPlaybackState, spotify.ScopeUserModifyPlaybackState, spotify.ScopePlaylistReadCollaborative, spotify.ScopePlaylistReadPrivate)
	auth.SetAuthInfo(spotifyClientID, spotifyClientSecret)

	p = &SpotifyConnectPlayer{
		sessionKey:    RandStringBytes(12),
		tokenFilePath: tokenFilePath,
		auth:          &auth,
	}

	// Add our own after authorisation handler
	p.AddAuthorisationListener(p.afterAuthorisation)

	go func() {
		err = http.ListenAndServe(fmt.Sprintf(":%d", authoriseHTTPPort), p)
		if err != nil {
			log.Printf("[SpotifyConnect] Error, could not start HTTP server on port %d: %v\n", authoriseHTTPPort, err)
			return
		}
	}()

	token, readErr := p.readToken()
	if readErr == nil {
		client := p.auth.NewClient(token)
		var userErr error
		p.user, userErr = client.CurrentUser()
		if userErr == nil {
			log.Printf("[SpotifyConnect] Reusing previous token")
			p.client = &client
			p.afterAuthorisation()
			return
		}
		log.Printf("[SpotifyConnect] Previous token invalid, reauthenticatation needed")
	}

	authURL = auth.AuthURL(p.sessionKey)
	log.Printf("[SpotifyConnect] Please authorise the MusicBot by visiting the following page in your browser: %s\n", authURL)
	return
}

func (p *SpotifyConnectPlayer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.client != nil {
		w.WriteHeader(http.StatusPreconditionFailed)
		fmt.Fprint(w, "<h1>Already authenticated!</h1>")
		return
	}

	token, err := p.auth.Token(p.sessionKey, r)
	if err != nil {
		http.Error(w, "Could not get token", http.StatusForbidden)
		log.Printf("[SpotifyConnect] Error pulling token from callback: %v\n", err)
		return
	}
	if st := r.FormValue("state"); st != p.sessionKey {
		log.Printf("[SpotifyConnect] Error, state mismatch: %v != %v\n", st, p.sessionKey)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// use the token to get an authenticated client
	client := p.auth.NewClient(token)
	p.client = &client
	p.user, err = p.client.CurrentUser()

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "<h1>Login completed as %s</h1>", p.user.ID)

	p.writeToken(token)

	for _, l := range p.authListeners {
		l()
	}
}

func (p *SpotifyConnectPlayer) AddAuthorisationListener(listener func()) {
	p.authListeners = append(p.authListeners, listener)
}

func (p *SpotifyConnectPlayer) afterAuthorisation() {
	// Turn repeat off, as it interferes with the musicplayer
	repErr := p.client.Repeat("off")
	if repErr != nil {
		log.Printf("[SpotifyConnect] Error setting repeat setting: %v\n", repErr)
	}

	// Turn shuffle off
	shufErr := p.client.Shuffle(false)
	if shufErr != nil {
		log.Printf("[SpotifyConnect] Error setting shuffle setting: %v\n", shufErr)
	}
}

func (p *SpotifyConnectPlayer) writeToken(token *oauth2.Token) (err error) {
	if p.tokenFilePath == "" {
		err = errors.New("Tokenfilepath invalid")
		return
	}
	buf, err := json.Marshal(token)
	if err != nil {
		return
	}
	err = ioutil.WriteFile(p.tokenFilePath, buf, 0755)
	return
}

func (p *SpotifyConnectPlayer) readToken() (token *oauth2.Token, err error) {
	if p.tokenFilePath == "" {
		err = errors.New("Tokenfilepath invalid")
		return
	}
	buf, err := ioutil.ReadFile(p.tokenFilePath)
	if err != nil {
		return
	}
	token = &oauth2.Token{}
	err = json.Unmarshal(buf, token)
	return
}

func (p *SpotifyConnectPlayer) Name() (name string) {
	return "Spotify"
}

func (p *SpotifyConnectPlayer) CanPlay(url string) (canPlay bool) {
	_, id, _, err := GetSpotifyTypeAndIDFromURL(url)
	canPlay = err == nil && id != ""
	return
}

func (p *SpotifyConnectPlayer) GetSongs(url string) (songs []Playable, err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	tp, id, userID, err := GetSpotifyTypeAndIDFromURL(url)
	if err != nil {
		err = fmt.Errorf("[SpotifyConnectPlayer] Could not parse URL: %v", err)
		return
	}
	switch tp {
	case TYPE_TRACK:
		var track *spotify.FullTrack
		track, err = p.client.GetTrack(spotify.ID(id))
		if err != nil {
			err = fmt.Errorf("[SpotifyConnectPlayer] Could not get track data for URL: %v", err)
			return
		}

		songs = append(songs,
			NewSong(GetSpotifyTrackName(&track.SimpleTrack), track.TimeDuration(), string(track.URI), GetSpotifyAlbumImage(&track.Album)),
		)
	case TYPE_ALBUM:
		var album *spotify.FullAlbum
		album, err = p.client.GetAlbum(spotify.ID(id))
		if err != nil {
			err = fmt.Errorf("[SpotifyConnectPlayer] Could not get album for URL: %v", err)
			return
		}
		for _, track := range album.Tracks.Tracks {
			songs = append(songs,
				NewSong(GetSpotifyTrackName(&track), track.TimeDuration(), string(track.URI), GetSpotifyAlbumImage(&album.SimpleAlbum)),
			)
		}
	case TYPE_PLAYLIST:
		var listTracks *spotify.PlaylistTrackPage
		listTracks, err = p.client.GetPlaylistTracks(userID, spotify.ID(id))
		if err != nil {
			err = fmt.Errorf("[SpotifyConnectPlayer] Could not get playlist tracks for URL: %v", err)
			return
		}
		for _, track := range listTracks.Tracks {
			if strings.HasPrefix(string(track.Track.URI), "spotify:local:") {
				continue
			}
			songs = append(songs,
				NewSong(GetSpotifyTrackName(&track.Track.SimpleTrack), track.Track.TimeDuration(), string(track.Track.URI), GetSpotifyAlbumImage(&track.Track.Album)),
			)
		}
	}
	return
}

func (p *SpotifyConnectPlayer) Search(searchType SearchType, searchStr string, limit int) (results []PlayableSearchResult, err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	results, err = GetSpotifySearchResults(p.client, searchType, searchStr, limit)
	return
}

func (p *SpotifyConnectPlayer) Play(url string) (err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	URI := spotify.URI(url)
	err = p.client.PlayOpt(&spotify.PlayOptions{
		URIs: []spotify.URI{URI},
	})
	return
}

func (p *SpotifyConnectPlayer) Seek(positionSeconds int) (err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	err = p.client.Seek(positionSeconds * 1000)
	return
}

func (p *SpotifyConnectPlayer) Pause(pauseState bool) (err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	if pauseState {
		err = p.client.Pause()
		return
	}
	err = p.client.Play()
	return
}

func (p *SpotifyConnectPlayer) Stop() (err error) {
	if p.client == nil {
		err = ErrNotAuthorised
		return
	}

	err = p.client.Pause()
	return
}
