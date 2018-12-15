package swgohhelp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/ronoaldo/swgoh/cache"
)

var errNotImplemented = fmt.Errorf("swgohhelp: not implemented")

// DefaultEndpoint is the default target host for API calls
var DefaultEndpoint = "https://api.swgoh.help"

// Client implements an authenticated callee to the https://api.swgoh.help service.
type Client struct {
	hc       *http.Client
	endpoint string
	token    string
	debug    bool
	gameData cache.Cache
	players  cache.Cache
}

// New initializes an instance of Client making it ready to use.
func New(ctx context.Context) *Client {
	client := &Client{
		hc:       http.DefaultClient,
		endpoint: DefaultEndpoint,
	}
	cacheDir, err := cacheDirectory()
	if err != nil {
		log.Printf("swgohhelp: error loading cache directory: %v", err)
	}
	client.gameData = cache.NewCache(path.Join(cacheDir, GameDataCacheFile), GameDataCacheExpiration)
	client.players = cache.NewCache(path.Join(cacheDir, PlayerCacheFile), PlayerCacheExpiration)
	return client
}

// SetDebug defines the debug state for the client.
func (c *Client) SetDebug(debug bool) *Client {
	c.debug = debug
	return c
}

// call internally makes and logs http requests to the API endpoints.
func (c *Client) call(method, urlPath, contentType string, body io.Reader, args ...interface{}) (resp *http.Response, err error) {
	url := fmt.Sprintf(c.endpoint+urlPath, args...)

	req, err := http.NewRequest(method, url, body)
	req.Header.Set("Content-type", contentType)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if err != nil {
		return nil, err
	}

	if c.debug {
		b, _ := httputil.DumpRequestOut(req, true)
		writeLogFile(b, "req", method, urlPath)
	}

	resp, err = c.hc.Do(req)
	if err != nil {
		return nil, err
	}

	if c.debug {
		b, _ := httputil.DumpResponse(resp, true)
		writeLogFile(b, "resp", method, urlPath)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("swgohhelp: unexpected status code calling %s: %d %s", url, resp.StatusCode, resp.Status)
	}

	return resp, nil
}

// SignIn authenticates the client and returns the accessToken or an error if authentication fails.
func (c *Client) SignIn(username, password string) (accessToken string, err error) {
	body := fmt.Sprintf("username=%s&password=%s&grant_type=password&client_id=goapiclient&client_secret=123456", username, password)
	resp, err := c.call("POST", "/auth/signin", "application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	var authResponse AuthResponse
	if err = json.NewDecoder(resp.Body).Decode(&authResponse); err != nil {
		return "", err
	}
	// Refresh token with the desired one
	c.token = authResponse.AccessToken
	return authResponse.AccessToken, nil
}

// Players retrieves several player profile stats and roster details.
func (c *Client) Players(allyCodes ...string) (players []Player, err error) {
	allyCodeNumbers, err := parseAllyCodes(allyCodes...)
	if err != nil {
		return nil, fmt.Errorf("swgohhelp: error parsing ally codes: %v", err)
	}
	// Check if we have some of them in cache first
	missingFromCache := make([]int, 0, len(allyCodeNumbers))
	for _, ally := range allyCodeNumbers {
		var player Player
		if ok := c.players.Get(strconv.Itoa(ally), &player); ok {
			players = append(players, player)
			continue
		}
		missingFromCache = append(missingFromCache, ally)
	}
	if len(missingFromCache) == 0 {
		return players, nil
	}
	payload, err := json.Marshal(map[string]interface{}{
		"allycodes": missingFromCache,
		"language":  "eng_us",
		"enums":     false,
		"project": map[string]int{
			"id":         1,
			"allyCode":   1,
			"name":       1,
			"level":      1,
			"stats":      1,
			"arena":      1,
			"roster":     1,
			"guildName":  1,
			"guildRefId": 1,
			"titles":     1,
			"updated":    1,
		},
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.call("POST", "/swgoh/player", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&players)
	if err != nil {
		return nil, err
	}

	// Enrich result with related data from data collections
	titles, err := c.DataPlayerTitles()
	if err != nil {
		return nil, err
	}
	for i := range players {
		players[i].Titles.Selected = titles[players[i].Titles.Selected].Name
		for j := range players[i].Titles.Unlocked {
			titleKey := players[i].Titles.Unlocked[j]
			players[i].Titles.Unlocked[j] = titles[titleKey].Name
		}
	}
	// Enrich result with related data from Crinolo's stat API
	url := "https://crinolo-swgoh.glitch.me/statCalc/api/characters/?flags=withModCalc,gameStyle"
	for i := range players {
		player := players[i]
		b, err := json.Marshal(player.Roster)
		if err != nil {
			return nil, err
		}
		if c.debug {
			writeLogFile(b, "req", "POST", "_crinoloapi")
		}
		resp, err := c.hc.Post(url, "application/json", bytes.NewBuffer(b))
		if err != nil {
			return nil, err
		}
		b, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if c.debug {
			writeLogFile(b, "resp", "POST", "_crinoloapi")
		}
		err = json.Unmarshal(b, &player.Roster)
		if err != nil {
			return nil, err
		}
	}

	// Save players missing form cache
	for i := range players {
		player := players[i]
		c.players.Put(strconv.Itoa(player.AllyCode), &player)
		log.Printf("swgohhelp: saving player %v in cache ...", player.AllyCode)
	}

	return players, nil
}

// writeLogFile is a debug helper function to write log data.
func writeLogFile(b []byte, reqresp, method, urlPath string) {
	urlPath = strings.Replace(urlPath, "/", "_", -1)
	fname := path.Join(os.TempDir(), fmt.Sprintf("swgohhelp%s-%s-%s.log", urlPath, method, reqresp))
	log.Printf("swgohhelp: writing log file %s: result: %v", fname, ioutil.WriteFile(fname, b, 0644))
}

var allyCodeCleanup = regexp.MustCompile("[^0-9]")

// parseAllyCodes takes several ally code as strings and returns integer equivalents.
func parseAllyCodes(allyCodes ...string) (allyCodeNumbers []int, err error) {
	for _, a := range allyCodes {
		n, err := strconv.Atoi(allyCodeCleanup.ReplaceAllString(a, ""))
		if err != nil {
			return nil, err
		}
		allyCodeNumbers = append(allyCodeNumbers, n)
	}
	return
}
