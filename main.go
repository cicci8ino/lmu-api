package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
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
	mu             sync.RWMutex
	races          []*lmuapi.Race
	scheduledRaces []*lmuapi.RaceSchedule
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

func parsePage(ctx context.Context, url string) ([]*lmuapi.Race, []*lmuapi.RaceSchedule, error) {
	log.Println("parsing page")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, err
	}

	container := doc.Find("#daily_lmu_race_list") // div id="daily_lmu_race_list"
	if container.Length() == 0 {
		return nil, nil, errors.New("daily_lmu_race_list not found")
	}

	loc, _ := time.LoadLocation("Europe/Rome")
	var res []*lmuapi.Race
	var schedule_res []*lmuapi.RaceSchedule

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
			for _, timestamp := range ts {
				race := &lmuapi.Race{
					Name:     name,
					Level:    level,
					Duration: parseDurationMinutes(durRaw),
					Track:    track,
					Schedule: timestamp,
				}
				//log.Println(race)
				res = append(res, race)
			}

			race_schedule := &lmuapi.RaceSchedule{Race: &lmuapi.Race{Name: name,
				Level:    level,
				Duration: parseDurationMinutes(durRaw),
				Track:    track}, Schedule: ts}
			schedule_res = append(schedule_res, race_schedule)
		}
	})

	return res, schedule_res, nil
}

func reload(ctx context.Context, url string) {
	data, scheduleData, err := parsePage(ctx, url)

	//inefficiend but who cares
	sort.Slice(data, func(i, j int) bool {
		return data[i].Schedule.AsTime().Before(data[j].Schedule.AsTime())
	})

	if err != nil {
		log.Println("parse error:", err)
		return
	}
	mu.Lock()
	races = data
	scheduledRaces = scheduleData
	mu.Unlock()
	removeOldRaces(ctx)
}

func removeOldRaces(ctx context.Context) {
	log.Println("Removing old races")
	mu.Lock()
	now := time.Now()
	races = slices.DeleteFunc(races, func(r *lmuapi.Race) bool {

		res := r.Schedule.AsTime().Before(now)
		return res
	})
	mu.Unlock()
}

func pageReloadScheduler(ctx context.Context, url string, every time.Duration) {
	log.Println("Reloading page")
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

func scheduleReloadScheduler(ctx context.Context) {
	removeOldRaces(ctx)
	now := time.Now()
	first := now.Truncate(10 * time.Minute).Add(10 * time.Minute)
	time.Sleep(time.Until(first))
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			removeOldRaces(ctx)
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

func (lm *LMU) GetRaceSchedule(context.Context, *lmuapi.GetRaceScheduleRequest) (*lmuapi.GetScheduleResponse, error) {
	return &lmuapi.GetScheduleResponse{RaceSchedule: scheduledRaces}, nil
}

func main() {
	var every time.Duration
	flag.DurationVar(&every, "interval", time.Hour, "update interval")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pageReloadScheduler(ctx, "https://www.racecontrol.gg", every)
	go scheduleReloadScheduler(ctx)

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
