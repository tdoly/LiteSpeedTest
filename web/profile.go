package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xxf098/lite-proxy/common"
	"github.com/xxf098/lite-proxy/download"
	"github.com/xxf098/lite-proxy/request"
)

var ErrInvalidData = errors.New("invalid data")

// support proxy
// concurrency setting
// as subscription server
// profiles filter
func getSubscriptionLinks(link string) ([]string, error) {
	c := http.Client{
		Timeout: 20 * time.Second,
	}
	resp, err := c.Get(link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	msg, err := common.DecodeB64(string(data))
	if err != nil {
		return nil, err
	}
	return parseLinks(msg)
}

func parseLinks(message string) ([]string, error) {
	// splits := strings.SplitN(string(message), "^", 2)
	// if len(splits) < 1 {
	// 	return nil, errors.New("Invalid Data")
	// }
	matched, err := regexp.MatchString(`^(?:https?:\/\/)(?:[^@\/\n]+@)?(?:www\.)?([^:\/\n]+)`, message)
	if matched && err == nil {
		return getSubscriptionLinks(message)
	}
	reg := regexp.MustCompile(`((?i)(vmess|ssr)://[a-zA-Z0-9+_/=-]+)|((?i)(ss|trojan)://(.+?)@(.+?):([0-9]{2,5})([?#][^\s]+))`)
	matches := reg.FindAllStringSubmatch(message, -1)
	links := make([]string, len(matches))
	for index, match := range matches {
		links[index] = match[0]
	}
	if len(links) < 1 {
		return nil, ErrInvalidData
	}
	return links, nil
}

func parseOptions(message string) (*ProfileTestOptions, error) {
	opts := strings.Split(message, "^")
	if len(opts) < 7 {
		return nil, ErrInvalidData
	}
	groupName := opts[0]
	if groupName == "?empty?" || groupName == "" {
		groupName = "Default Group"
	}
	concurrency, err := strconv.Atoi(opts[5])
	if err != nil {
		return nil, err
	}
	if concurrency < 1 {
		concurrency = 1
	}
	timeout, err := strconv.Atoi(opts[6])
	if err != nil {
		return nil, err
	}
	if timeout < 20 {
		timeout = 20
	}
	testOpt := &ProfileTestOptions{
		GroupName:     groupName,
		SpeedTestMode: opts[1],
		PingMethod:    opts[2],
		SortMethod:    opts[3],
		Concurrency:   concurrency,
		TestMode:      ALLTEST,
		Timeout:       time.Duration(timeout) * time.Second,
	}
	return testOpt, nil
}

const (
	SpeedOnly = "speedonly"
	PingOnly  = "pingonly"
	ALLTEST   = iota
	RETEST
)

type ProfileTestOptions struct {
	GroupName     string        `json:"group"`
	SpeedTestMode string        `json:"speedtestMode"`
	PingMethod    string        `json:"pingMethod"`
	SortMethod    string        `json:"sortMethod"`
	Concurrency   int           `json:"concurrency"`
	TestMode      int           `json:"testMode"`
	TestIDs       []int         `json:"testids"`
	Timeout       time.Duration `json:"timeout"`
	Links         []string      `json:"links"`
}

func parseMessage(message []byte) ([]string, *ProfileTestOptions, error) {
	links, options, err := parseRetestMessage(message)
	if err == nil {
		return links, options, err
	}
	splits := strings.SplitN(string(message), "^", 2)
	if len(splits) < 2 {
		return nil, nil, ErrInvalidData
	}
	links, err = parseLinks(splits[0])
	if err != nil {
		return nil, nil, err
	}
	options, err = parseOptions(splits[1])
	if err != nil {
		return nil, nil, err
	}
	return links, options, nil
}

func parseRetestMessage(message []byte) ([]string, *ProfileTestOptions, error) {
	options := &ProfileTestOptions{}
	err := json.Unmarshal(message, options)
	if err != nil {
		return nil, nil, err
	}
	if options.TestMode != RETEST {
		return nil, nil, errors.New("not retest mode")
	}
	options.TestMode = RETEST
	options.Timeout = time.Duration(int(options.Timeout)) * time.Second
	if options.GroupName == "?empty?" || options.GroupName == "" {
		options.GroupName = "Default Group"
	}
	if options.Timeout < 20 {
		options.Timeout = 20
	}
	if options.Concurrency < 1 {
		options.Concurrency = 1
	}
	return options.Links, options, nil
}

type ProfileTest struct {
	Conn        *websocket.Conn
	Options     *ProfileTestOptions
	MessageType int
	Links       []string
	mu          sync.Mutex
	wg          sync.WaitGroup // wait for all to finish
}

func (p *ProfileTest) WriteMessage(data []byte) error {
	p.mu.Lock()
	err := p.Conn.WriteMessage(p.MessageType, data)
	p.mu.Unlock()
	return err
}

func (p *ProfileTest) WriteString(data string) error {
	b := []byte(data)
	return p.WriteMessage(b)
}

func (p *ProfileTest) testAll(ctx context.Context) error {
	if len(p.Links) < 1 {
		p.WriteString(SPEEDTEST_ERROR_NONODES)
		return fmt.Errorf("no profile found")
	}
	p.WriteMessage(getMsgByte(-1, "started"))
	for i := range p.Links {
		p.WriteMessage(gotserverMsg(i, p.Links[i], p.Options.GroupName))
	}
	guard := make(chan int, p.Options.Concurrency)
	for i := range p.Links {
		p.wg.Add(1)
		id := i
		link := ""
		if len(p.Options.TestIDs) > 0 && len(p.Options.Links) > 0 {
			id = p.Options.TestIDs[i]
			link = p.Options.Links[i]
		}
		select {
		case guard <- i:
			go func(id int, link string, c <-chan int) {
				p.testOne(ctx, id, link)
				_ = p.WriteMessage(getMsgByte(id, "endone"))
				<-c
			}(id, link, guard)
		case <-ctx.Done():
			return nil
		}
	}
	p.wg.Wait()
	p.WriteMessage(getMsgByte(-1, "eof"))
	return nil
}

func (p *ProfileTest) testOne(ctx context.Context, index int, link string) error {
	// panic
	if link == "" {
		defer p.wg.Done()
		link = p.Links[index]
		link = strings.SplitN(link, "^", 2)[0]
	}
	err := p.pingLink(index, link)
	if err != nil {
		return err
	}
	err = p.WriteMessage(getMsgByte(index, "startspeed"))
	ch := make(chan int64, 1)
	defer close(ch)
	go func(ch <-chan int64) {
		var max int64
		var speeds []int64
		for {
			select {
			case speed, ok := <-ch:
				if !ok || speed < 0 {
					return
				}
				speeds = append(speeds, speed)
				var avg int64
				for _, s := range speeds {
					avg += s / int64(len(speeds))
				}
				if max < speed {
					max = speed
				}
				_, remarks, err := getRemarks(link)
				if err != nil {
					remarks = fmt.Sprintf("Profile %d", index)
				}
				log.Printf("%s recv: %s", remarks, download.ByteCountIEC(speed))
				err = p.WriteMessage(getMsgByte(index, "gotspeed", avg, max, speed))
			case <-ctx.Done():
				log.Printf("index %d done!", index)
				return
			}
		}
	}(ch)
	speed, err := download.Download(link, p.Options.Timeout, p.Options.Timeout, ch)
	if speed < 1 {
		p.WriteMessage(getMsgByte(index, "gotspeed", -1, -1, 0))
	}
	return err
}

func (p *ProfileTest) pingLink(index int, link string) error {
	if p.Options.SpeedTestMode == SpeedOnly {
		return nil
	}
	if link == "" {
		link = p.Links[index]
	}
	p.WriteMessage(getMsgByte(index, "startping"))
	elapse, err := request.PingLink(link, 2)
	p.WriteMessage(getMsgByte(index, "gotping", elapse))
	if elapse < 1 {
		p.WriteMessage(getMsgByte(index, "gotspeed", -1, -1, 0))
		return err
	}
	if p.Options.SpeedTestMode == PingOnly {
		p.WriteMessage(getMsgByte(index, "gotspeed", -1, -1, 0))
		return errors.New(PingOnly)
	}
	return err
}
