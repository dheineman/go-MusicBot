package main

import (
	"fmt"
	"github.com/SvenWiltink/go-MusicBot/api"
	"github.com/SvenWiltink/go-MusicBot/bot"
	"github.com/SvenWiltink/go-MusicBot/config"
	"github.com/SvenWiltink/go-MusicBot/player"
	"github.com/SvenWiltink/go-MusicBot/songplayer"
	"github.com/SvenWiltink/go-MusicBot/util"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	conf, err := config.ReadConfig("conf.json")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	queueStorage := config.NewQueueStorage(conf.QueuePath)
	playr := player.NewPlayer()

	// Initialize the API
	apiObject := api.NewAPI(&conf.API, playr)
	go apiObject.Start()

	// Initialize the IRC bot
	musicBot, err := bot.NewMusicBot(&conf.IRC, playr)
	if err != nil {
		fmt.Printf("Error creating IRC bot: %v\n", err)
		return
	}
	err = musicBot.Start()
	if err != nil {
		fmt.Printf("Error starting IRC bot: %v\n", err)
		return
	}

	if conf.YoutubePlayer.Enabled {
		ytPlayer, err := songplayer.NewYoutubePlayer(conf.YoutubePlayer.YoutubeAPIKey, conf.YoutubePlayer.MpvBinPath, conf.YoutubePlayer.MpvInputPath)
		if err != nil {
			fmt.Printf("Error creating Youtube player: %v\n", err)

			musicBot.Announce(fmt.Sprintf("[YoutubePlayer] Error creating player: %v", err))
		} else {
			playr.AddSongPlayer(ytPlayer)
			fmt.Println("Added Youtube player")
		}
	}

	if conf.SpotifyPlayer.Enabled && conf.SpotifyPlayer.UseConnect {
		spPlayer, authURL, err := songplayer.NewSpotifyConnectPlayer(conf.SpotifyPlayer.ClientID, conf.SpotifyPlayer.ClientSecret, conf.SpotifyPlayer.TokenFilePath, "", 0)
		if err != nil {
			fmt.Printf("Error creating SpotifyConnect player: %v\n", err)

			musicBot.Announce(fmt.Sprintf("[SpotifyConnect] Error creating player: %v", err))
		} else if authURL != "" {
			ips, err := util.GetExternalIPs()
			ipStr := "???"
			if err != nil {
				fmt.Printf("Error getting external IPs: %v\n", err)
			} else {
				ipStr = ""
				for _, ip := range ips {
					ipStr += ip.String() + " "
				}
				ipStr = strings.TrimSpace(ipStr)
			}
			musicBot.Announce(fmt.Sprintf("[SpotifyConnect] Authorisation: Add the external IP (%s) of the bot to your hosts file under 'musicbot' and visit: %s", ipStr, authURL))

			spPlayer.AddAuthorisationListener(func() {
				playr.AddSongPlayer(spPlayer)
				fmt.Println("Added SpotifyConnect player")

				musicBot.Announce("[SpotifyConnect] The musicbot was successfully authorised!")
			})
		} else {
			playr.AddSongPlayer(spPlayer)
			fmt.Println("Added SpotifyConnect player")
		}
	} else if conf.SpotifyPlayer.Enabled && !conf.SpotifyPlayer.UseConnect {
		spPlayer, err := songplayer.NewSpotifyPlayer(conf.SpotifyPlayer.Host)
		if err != nil {
			fmt.Printf("Error creating Spotify player: %v\n", err)

			musicBot.Announce(fmt.Sprintf("[Spotify] Error creating player: %v", err))
		} else {
			playr.AddSongPlayer(spPlayer)
			fmt.Println("Added Spotify player")
		}
	}

	urls, err := queueStorage.ReadQueue()
	if err != nil {
		fmt.Printf("Error reading queue file: %v\n", err)

		musicBot.Announce(fmt.Sprintf("[Queue] Error loading queue: %v", err))
	} else {
		for _, url := range urls {
			playr.AddSongs(url)
		}
	}
	playr.AddListener("queue_updated", queueStorage.OnListUpdate)

	// Wait for a terminate signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	<-sigs
	musicBot.Stop()
}
