package songplayer

import (
	"errors"
	"fmt"
	"github.com/DexterLB/mpvipc"
	"github.com/vansante/go-event-emitter"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var youtubeURLRegex, _ = regexp.Compile(`^(https?://)?(www\.)?(youtube\.com|youtu\.?be)/.+$`)

const (
	MPV_INIT_RETRY_ATTEMPTS = 5
	MAX_MPV_LOAD_WAIT       = time.Duration(time.Second * 20)
)

type YoutubePlayer struct {
	mpvBinPath   string
	mpvInputPath string

	mpvProcess   *exec.Cmd
	mpvIsRunning bool
	mpvConn      *mpvipc.Connection
	ytAPI        *YouTubeAPI
	mpvMutex     sync.Mutex
	mpvEvents    *eventemitter.Emitter
}

func NewYoutubePlayer(youtubeAPIKey, mpvBinPath, mpvInputPath string) (player *YoutubePlayer, err error) {
	if youtubeAPIKey == "" {
		err = errors.New("Youtube API key is empty")
		return
	}

	if mpvBinPath == "" {
		mpvBinPath = "mpv"
	}
	if mpvInputPath == "" {
		mpvInputPath = ".mpv-input"
	}
	player = &YoutubePlayer{
		mpvBinPath:   mpvBinPath,
		mpvInputPath: mpvInputPath,
		mpvIsRunning: false,
		ytAPI:        NewYoutubeAPI(youtubeAPIKey),
		mpvEvents:    eventemitter.NewEmitter(),
	}

	err = player.init()
	return
}

func (p *YoutubePlayer) init() (err error) {
	p.mpvMutex.Lock()
	defer p.mpvMutex.Unlock()

	fi, err := os.Stat(p.mpvInputPath)
	if err == nil && !fi.IsDir() {
		fmt.Printf("[YoutubePlayer] Removing existing MPV control node on %s\n", p.mpvInputPath)
		err = os.Remove(p.mpvInputPath)
		if err != nil {
			err = fmt.Errorf("[YoutubePlayer] Error removing existing input %s: %v", p.mpvInputPath, err)
			return
		}
	}

	err = p.startMpv()
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error starting mpv: %v ", err)
		return
	}

	attempts := 0
	for {
		// Give MPV a second or so to start and create the input socket
		time.Sleep(500 * time.Millisecond)

		fmt.Printf("[YoutubePlayer] Opening mpv ipc connection on %s\n", p.mpvInputPath)
		p.mpvConn = mpvipc.NewConnection(p.mpvInputPath)
		err = p.mpvConn.Open()
		if err != nil {
			if attempts >= MPV_INIT_RETRY_ATTEMPTS {
				err = fmt.Errorf("[YoutubePlayer] Error opening IPC connection on %s: %v ", p.mpvInputPath, err)
				return
			}
		} else {
			err = nil
			break
		}
		attempts++
	}

	// Turn on all events.
	p.mpvConn.Call("enable_event", "all")

	go func() {
		events, stopListening := p.mpvConn.NewEventListener()
		for event := range events {
			p.mpvEvents.EmitEvent(event.Name, event)
		}
		stopListening <- struct{}{}
	}()

	return
}

func (p *YoutubePlayer) startMpv() (err error) {
	fmt.Printf("[YoutubePlayer] Starting MPV %s with control %s in idle mode\n", p.mpvBinPath, p.mpvInputPath)

	command := exec.Command(p.mpvBinPath, "--no-video", "--idle", "--input-ipc-server="+p.mpvInputPath)
	p.mpvProcess = command

	err = command.Start()
	p.mpvIsRunning = err == nil

	if err != nil {
		return
	}

	go func() {
		err := command.Wait()

		fmt.Printf("[YoutubePlayer] mpv has quit: %v\n", err)

		p.mpvMutex.Lock()
		p.mpvIsRunning = false
		p.mpvProcess = nil
		p.mpvMutex.Unlock()
	}()
	return
}

func (p *YoutubePlayer) checkRunning() (err error) {
	if p.mpvIsRunning && p.mpvProcess != nil {
		return
	}
	fmt.Print("[YoutubePlayer] mpv is not running, restarting mpv\n")
	err = p.init()
	return
}

func (p *YoutubePlayer) Name() (name string) {
	return "Youtube"
}

func (p *YoutubePlayer) CanPlay(url string) (canPlay bool) {
	return youtubeURLRegex.MatchString(url)
}

func (p *YoutubePlayer) GetSongs(url string) (songs []Playable, err error) {
	lowerURL := strings.ToLower(url)
	if strings.Contains(lowerURL, "player") || strings.Contains(lowerURL, "list=") {
		songs, err = p.ytAPI.GetPlayablesForPlaylistURL(url)
		// On error, fall back to single add
		if err == nil {
			return
		}
	}

	song, err := p.ytAPI.GetPlayableForURL(url)
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error getting meta data: %v", err)
		return
	}
	songs = append(songs, song)
	return
}

func (p *YoutubePlayer) Search(searchType SearchType, searchStr string, limit int) (results []PlayableSearchResult, err error) {
	results, err = p.ytAPI.Search(searchType, searchStr, limit)
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error searching songs: %v", err)
	}
	return
}

func (p *YoutubePlayer) Play(url string) (err error) {
	p.mpvMutex.Lock()
	defer p.mpvMutex.Unlock()

	err = p.checkRunning()
	if err != nil {
		return
	}

	err = p.stop()
	if err != nil {
		return
	}

	waitForLoad := make(chan bool)
	p.mpvEvents.ListenOnce("file-loaded", func(arguments ...interface{}) {
		waitForLoad <- true
	})

	// Start an event listener to wait for the file to load.
	_, err = p.mpvConn.Call("loadfile", url, "replace")
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error sending loadfile command: %v", err)
		return
	}

	go func() {
		time.Sleep(MAX_MPV_LOAD_WAIT)
		waitForLoad <- false
	}()

	success := <-waitForLoad
	if !success {
		fmt.Printf("[YoutubePlayer] Load file timeout\n")
		_, err = p.mpvConn.Call("stop")
		if err != nil {
			fmt.Printf("[YoutubePlayer] Error calling stop after timeout: %v\n", err)
		}
		err = fmt.Errorf("[YoutubePlayer] Error loading file, mpv did not respond in time")
		return
	}
	return
}

func (p *YoutubePlayer) Seek(positionSeconds int) (err error) {
	p.mpvMutex.Lock()
	defer p.mpvMutex.Unlock()

	err = p.checkRunning()
	if err != nil {
		return
	}

	err = p.mpvConn.Set("time-pos", positionSeconds)
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error sending time-pos property: %v", err)
	}
	return
}

func (p *YoutubePlayer) Pause(pauseState bool) (err error) {
	p.mpvMutex.Lock()
	defer p.mpvMutex.Unlock()

	err = p.checkRunning()
	if err != nil {
		return
	}

	err = p.mpvConn.Set("pause", pauseState)
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error sending pause state property: %v", err)
	}
	return
}

func (p *YoutubePlayer) Stop() (err error) {
	p.mpvMutex.Lock()
	defer p.mpvMutex.Unlock()

	return p.stop()
}

func (p *YoutubePlayer) stop() (err error) {
	err = p.checkRunning()
	if err != nil {
		return
	}

	_, err = p.mpvConn.Call("stop")
	if err != nil {
		err = fmt.Errorf("[YoutubePlayer] Error sending stop command: %v", err)
	}
	return
}
