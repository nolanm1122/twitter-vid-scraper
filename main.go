package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/go-errors/errors"
	"github.com/tidwall/gjson"
	"github.com/ztrue/tracerr"
	"gopkg.in/resty.v1"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"utils"
)

const (
	AuthToken = "AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"
	AuthHeader = "Bearer " + AuthToken
	UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/85.0.4183.121 Safari/537.36"
)

var (
	BaseUrl = utils.MustParseUrl("https://twitter.com")
	gtCookieReg = regexp.MustCompile(`decodeURIComponent\("gt=(\d+);`)

	defaultScrapeParams = map[string]string {
		"include_profile_interstitial_type": "1",
		"include_blocking": "1",
		"include_blocked_by": "1",
		"include_followed_by": "1",
		"include_want_retweets": "1",
		"include_mute_edge": "1",
		"include_can_dm": "1",
		"include_can_media_tag": "1",
		"skip_status": "1",
		"cards_platform": "Web-12",
		"include_cards": "1",
		"include_ext_alt_text": "true",
		"include_quote_count": "true",
		"include_reply_count": "1",
		"tweet_mode": "extended",
		"include_entities": "true",
		"include_user_entities": "true",
		"include_ext_media_color": "true",
		"include_ext_media_availability": "true",
		"send_error_codes": "true",
		"simple_quoted_tweet": "true",
		"count": "20",
		"ext": "mediaStats,highlightedLabel",
	}
)

func makeClient() *resty.Client {

	return resty.New().SetHeader("user-agent", UserAgent).SetRetryCount(3)
}

func initClient(c *resty.Client) error {
	gtFound := false
	var r *resty.Response
	var err error
	for !gtFound {
		r, err = c.R().Get(BaseUrl.String())
		if err != nil {
			return tracerr.Wrap(err)
		}
		gtFound = strings.Contains(r.String(), "gt=")
	}
	gtVal := gtCookieReg.FindStringSubmatch(r.String())[1]
	c.GetClient().Jar.SetCookies(BaseUrl, []*http.Cookie{{Name: "gt", Value: gtVal, Path: "/", Domain: ".twitter.com"}})
	c.SetHeader("x-guest-token", gtVal)
	r, err = c.R().Get("https://twitter.com/i/js_inst?c_name=ui_metrics")
	if err != nil {
		return tracerr.Wrap(err)
	}
	return nil
}

func getUserID(screenName string, c *resty.Client) (int, error) {
	variables := map[string]interface{}{"screen_name": screenName, "withHighlightedLabel": true}
	j, err := json.Marshal(variables)
	if err != nil {
		return 0, tracerr.Wrap(err)
	}
	r, err := c.R().SetQueryParam("variables", string(j)).SetHeader("Authorization", AuthHeader).Get("https://api.twitter.com/graphql/-xfUfZsnR_zqjFd-IfrN5A/UserByScreenName")
	if err != nil {
		return 0, tracerr.Wrap(err)
	}
	if r.IsError() {
		return 0, errors.New("get user id bad response: " + r.String())
	}
	return int(gjson.Get(r.String(), "data.user.rest_id").Int()), nil
}

func defaultScrapeParamsCopy() map[string]string {
	m := make(map[string]string)
	for k, v := range defaultScrapeParams {
		m[k] = v
	}
	return m
}


func scrapeUser(userID int, c *resty.Client) (<- chan string, chan <- bool, <- chan error) {
	urlCh := make(chan string)
	errCh := make(chan error)
	doneCh := make(chan bool, 1)

	go func() {
		defer func() {
			close(urlCh)
			close(errCh)
			close(doneCh)
		}()
		params := defaultScrapeParamsCopy()
		var done = false
		for !done {
			r, err := c.R().SetQueryParams(params).SetHeader("Authorization", AuthHeader).Get(fmt.Sprintf("https://api.twitter.com/2/timeline/media/%d.json", userID))
			if err != nil {
				errCh <- tracerr.Wrap(err)
				return
			}
			tweets := gjson.Get(r.String(), "globalObjects.tweets")
			tweets.ForEach(func(key, value gjson.Result) bool {
				if done{
					return false
				}
				medias := value.Get("extended_entities.media")
				medias.ForEach(func(key, media gjson.Result) bool {
					if done {
						return false
					}
					if media.Get("type").Str != "video" {
						return true
					}
					select {
					case <- doneCh:
						done = true
						return false
					default:
						urlCh <- media.Get("display_url").Str
						return true
					}
				})
				return true
			})
			if done {
				return
			}
			entries := gjson.Get(r.String(), "timeline.instructions.0.addEntries.entries").Array()
			if len(entries) < 1 {
				return
			}
			last := entries[len(entries) - 1]
			cursor := last.Get("content.operation.cursor.value").Str
			if cursor == "" {
				return
			}
			params["cursor"] = cursor
		}
	}()
	return urlCh, doneCh, errCh
}

func promptGetResponse(prompt string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(prompt)
	text, _ := reader.ReadString('\n')
	return strings.TrimFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
}


func main() {
	screenName := promptGetResponse("[?] Enter Twitter screen name: ")
	numStr := promptGetResponse("[?] Enter max number to scrape: ")
	num, err := strconv.Atoi(numStr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("[*] Initializing...")
	c := makeClient()
	err = initClient(c)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Done.")
	id, err := getUserID(screenName, c)
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(fmt.Sprintf("./%s.csv", screenName))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	urlCh, doneCh, _ := scrapeUser(id, c)
	count := 0
	for url := range urlCh {
		count++
		f.WriteString(fmt.Sprintf("%s,,\n", url))
		f.Sync()
		if count >= num {
			doneCh <- true
			fmt.Printf("[*] %d/%d\n", count, num)
			return
		}
		if count % 5 == 0 && count > 0 {
			fmt.Printf("[*] %d/%d\n", count, num)
		}
	}
}
