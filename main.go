package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	lmuapi "github.com/cicci8ino/lmu-api/api/gen"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	mu    sync.RWMutex
	races []*lmuapi.Race
)

func parseDurationMinutes(raw string) int32 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	total := 0
	fields := strings.Fields(raw)
	for i, f := range fields {
		switch {
		case strings.HasSuffix(f, "h"):
			h, _ := strconv.Atoi(strings.TrimSuffix(f, "h"))
			total += h * 60
		case f == "minutes" || f == "minute":
			if i > 0 {
				m, _ := strconv.Atoi(fields[i-1])
				total += m
			}
		}
	}
	return int32(total)
}

var reTimestamp = regexp.MustCompile(`\d{1,2} \w{3} at \d{1,2}:\d{2}[ap]m`)

func parseTimes(block string, loc *time.Location) []*timestamppb.Timestamp {
	matches := reTimestamp.FindAllString(block, -1)
	out := make([]*timestamppb.Timestamp, 0, len(matches))
	year := time.Now().Year()
	layout := "2 Jan at 3:04pm"
	for _, m := range matches {
		t, err := time.ParseInLocation(layout, m, loc)
		if err != nil {
			continue
		}
		t = t.AddDate(year-t.Year(), 0, 0)
		out = append(out, timestamppb.New(t))
	}
	return out
}

func parsePage(ctx context.Context, url string) ([]*lmuapi.Race, error) {
	log.Println("parsing page")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	container := doc.Find("#daily_lmu_race_list") // div id="daily_lmu_race_list"
	if container.Length() == 0 {
		return nil, errors.New("daily_lmu_race_list not found")
	}

	loc, _ := time.LoadLocation("Europe/Rome")
	var res []*lmuapi.Race

	container.Find("div.scheduled-race-card").Each(func(_ int, card *goquery.Selection) {
		level := strings.TrimSpace(card.Find(".tier-badge").Clone().Children().Remove().End().Text())
		name := strings.TrimSpace(card.Find("h4").First().Text())
		durRaw := strings.TrimSpace(card.Find(".race_header").Eq(0).Find("span").Last().Text())
		track := strings.TrimSpace(card.Find(".race_header").Eq(1).Find("span").Last().Text())

		var ts []*timestamppb.Timestamp
		card.Find(".marquee-content span").Each(func(_ int, s *goquery.Selection) {
			txt := strings.TrimSpace(s.Text())
			if txt == "" {
				return
			}
			dt, err := time.ParseInLocation("2 Jan at 3:04pm", txt, loc)
			if err != nil {
				return
			}
			year := time.Now().Year()
			dt = time.Date(year, dt.Month(), dt.Day(), dt.Hour(), dt.Minute(), 0, 0, loc)
			ts = append(ts, timestamppb.New(dt))
		})

		if name != "" {
			race := &lmuapi.Race{
				Name:     name,
				Level:    level,
				Duration: parseDurationMinutes(durRaw),
				Track:    track,
				Schedule: ts,
			}
			log.Println(race)
			res = append(res, race)
		}
	})

	return res, nil
}

func reload(ctx context.Context, url string) {
	data, err := parsePage(ctx, url)
	if err != nil {
		log.Println("parse error:", err)
		return
	}
	mu.Lock()
	races = data
	mu.Unlock()
}

func scheduler(ctx context.Context, url string, every time.Duration) {
	reload(ctx, url)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			reload(ctx, url)
		case <-ctx.Done():
			return
		}
	}
}

type LMU struct {
	lmuapi.UnimplementedRaceServiceServer
}

func (lm *LMU) GetRaces(context.Context, *lmuapi.GetRacesRequest) (*lmuapi.GetRacesResponse, error) {
	return &lmuapi.GetRacesResponse{Races: races}, nil
}

func main() {
	var every time.Duration
	flag.DurationVar(&every, "interval", 24*time.Hour, "update interval")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler(ctx, "https://www.racecontrol.gg", every)

	// gRPC server
	grpcLis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	lmuapi.RegisterRaceServiceServer(grpcServer, &LMU{})
	go func() { log.Fatal(grpcServer.Serve(grpcLis)) }()

	// gRPC-Gateway
	mux := runtime.NewServeMux()
	err = lmuapi.RegisterRaceServiceHandlerFromEndpoint(
		ctx,
		mux,
		"localhost:50051",
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	)
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(http.ListenAndServe(":8080", mux))
}
