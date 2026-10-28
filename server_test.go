package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMeetingRoomScheduleDoesNotForwardRequest(t *testing.T) {
	upstreamRequestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamRequestCount++
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	handler := newHTTPHandler(&dingTalkClient{appKey: "test-key", appSecret: "test-secret", baseURL: upstream.URL, httpClient: upstream.Client()}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/meeting-rooms/schedules", strings.NewReader(`{
		"userId":"user-1",
		"roomIds":["room-1"],
		"startTime":"2026-10-22T10:00:00+08:00",
		"endTime":"2026-10-22T11:00:00+08:00"
	}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusGone {
		t.Fatalf("expected status 410, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "direct DingTalk schedule queries are disabled") {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	if upstreamRequestCount != 0 {
		t.Fatalf("expected no DingTalk requests, got %d", upstreamRequestCount)
	}
}

func TestMeetingRoomScheduleRejectsInvalidRequest(t *testing.T) {
	handler := newHTTPHandler(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/meeting-rooms/schedules", strings.NewReader(`{
		"userId":"user-1",
		"roomIds":[],
		"startTime":"2026-10-22T10:00:00+08:00",
		"endTime":"2026-10-22T11:00:00+08:00"
	}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusGone {
		t.Fatalf("expected status 410, got %d: %s", response.Code, response.Body.String())
	}
}

func TestResolveMeetingRoomID(t *testing.T) {
	testCases := []struct {
		name       string
		roomNumber string
		roomID     string
		found      bool
	}{
		{name: "Inazuma", roomNumber: "1", roomID: "6a54b9f2d4b44318bfaeafe953d4914fb574d07771682d09", found: true},
		{name: "Fontaine", roomNumber: "2", roomID: "202892463ff2314d533490c228e6847eb574d07771682d09", found: true},
		{name: "Liyue", roomNumber: "3", roomID: "2d8e58ea71f66009492807e2949ba1edb574d07771682d09", found: true},
		{name: "Mondstadt", roomNumber: "4", roomID: "5d8e21795c33aa2502bc497bb1db3c81b574d07771682d09", found: true},
		{name: "Natlan", roomNumber: "5", roomID: "f8b00c2ed3e6ea2f36c8807a6ccbf605b574d07771682d09", found: true},
		{name: "Sumeru", roomNumber: "6", roomID: "88181f34d87dd7e67dbf33e867d3a59bb574d07771682d09", found: true},
		{name: "invalid", roomNumber: "7", found: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			roomID, found := resolveMeetingRoomID(testCase.roomNumber)
			if roomID != testCase.roomID || found != testCase.found {
				t.Fatalf("resolveMeetingRoomID(%q) = (%q, %t), want (%q, %t)", testCase.roomNumber, roomID, found, testCase.roomID, testCase.found)
			}
		})
	}
}

func TestRefreshDashboardScheduleCacheFetchesAllRooms(t *testing.T) {
	tokenRequestCount := 0
	scheduleRequestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1.0/oauth2/accessToken":
			tokenRequestCount++
			_, _ = writer.Write([]byte(`{"accessToken":"test-token","expireIn":7200}`))
		case "/v1.0/calendar/users/dashboard-user/meetingRooms/schedules/query":
			scheduleRequestCount++
			body, _ := io.ReadAll(request.Body)
			for _, roomID := range allMeetingRoomIDs() {
				if !strings.Contains(string(body), roomID) {
					t.Errorf("batch request did not include room ID %q: %s", roomID, body)
				}
			}
			_, _ = writer.Write([]byte(`{"meetingRoomSchedules":[
				{"roomId":"6a54b9f2d4b44318bfaeafe953d4914fb574d07771682d09","scheduleItems":[{"eventId":"event-1","start":{"dateTime":"2026-10-23T02:00:00Z"},"end":{"dateTime":"2026-10-23T03:00:00Z"},"organizer":{"id":"organizer-1"}}]},
				{"roomId":"202892463ff2314d533490c228e6847eb574d07771682d09","scheduleItems":[]},
				{"roomId":"2d8e58ea71f66009492807e2949ba1edb574d07771682d09","scheduleItems":[]},
				{"roomId":"5d8e21795c33aa2502bc497bb1db3c81b574d07771682d09","scheduleItems":[]},
				{"roomId":"f8b00c2ed3e6ea2f36c8807a6ccbf605b574d07771682d09","scheduleItems":[]},
				{"roomId":"88181f34d87dd7e67dbf33e867d3a59bb574d07771682d09","scheduleItems":[]}
			]}`))
		default:
			t.Errorf("unexpected path: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	location := time.FixedZone("UTC+8", 8*60*60)
	client := &dingTalkClient{
		appKey:          "test-key",
		appSecret:       "test-secret",
		baseURL:         upstream.URL,
		httpClient:      upstream.Client(),
		dashboardUserID: "dashboard-user",
		location:        location,
	}
	cache := &dashboardScheduleCache{}
	if err := refreshDashboardScheduleCache(context.Background(), client, cache, time.Date(2026, time.October, 23, 9, 0, 0, 0, location)); err != nil {
		t.Fatalf("refresh dashboard cache: %v", err)
	}

	if tokenRequestCount != 1 || scheduleRequestCount != 1 {
		t.Fatalf("expected one token request and one batch schedule request, got token=%d schedule=%d", tokenRequestCount, scheduleRequestCount)
	}
	schedules, _, ready := cache.load()
	if !ready || len(schedules.schedules()) != len(meetingRoomByNumber) {
		t.Fatalf("unexpected cache contents: ready=%t schedules=%d", ready, len(schedules.schedules()))
	}
	if err := refreshDashboardScheduleCache(context.Background(), client, cache, time.Date(2026, time.October, 23, 9, 5, 0, 0, location)); err != nil {
		t.Fatalf("refresh dashboard cache a second time: %v", err)
	}
	if tokenRequestCount != 1 || scheduleRequestCount != 2 {
		t.Fatalf("expected cached token and one schedule request per refresh, got token=%d schedule=%d", tokenRequestCount, scheduleRequestCount)
	}
}

func TestDashboardStatusUsesCachedSchedules(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Now().In(location)
	roomID, _ := resolveMeetingRoomID("1")
	cache := &dashboardScheduleCache{}
	cache.store(meetingRoomScheduleResponse{MeetingRoomSchedules: []meetingRoomSchedules{{
		MeetingRoomID: roomID,
		Schedules: []meetingSchedule{{
			EventID:   "event-1",
			Start:     meetingScheduleTime{DateTime: now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)},
			End:       meetingScheduleTime{DateTime: now.Add(10 * time.Minute).UTC().Format(time.RFC3339)},
			Summary:   "Current meeting",
			Organizer: meetingOrganizerRef{UserID: "user-1", DisplayName: "Owner"},
		}},
	}}}, now)

	handler := newHTTPHandler(&dingTalkClient{dashboardUserID: "dashboard-user", location: location}, cache)
	request := httptest.NewRequest(http.MethodGet, "/api/dingtalk/meeting-rooms/1/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"title":"Current meeting"`) {
		t.Fatalf("unexpected dashboard response: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"number":"1"`) || !strings.Contains(response.Body.String(), `"name":"稻妻(Inazuma)"`) {
		t.Fatalf("expected room metadata in dashboard response: %s", response.Body.String())
	}
}

func TestBuildMeetingRoomDashboardShowsUpcomingMeetingFromMeetingRoomSchedules(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	response, err := buildMeetingRoomDashboard(meetingRoomScheduleResponse{MeetingRoomSchedules: []meetingRoomSchedules{{
		MeetingRoomID: "room-1",
		Schedules: []meetingSchedule{{
			EventID:   "event-1",
			Start:     meetingScheduleTime{DateTime: "2026-10-23T02:00:00Z"},
			End:       meetingScheduleTime{DateTime: "2026-10-23T03:00:00Z"},
			Summary:   "Project review",
			Organizer: meetingOrganizerRef{UserID: "booker-1", DisplayName: "Owner"},
		}},
	}}}, "room-1", time.Date(2026, time.October, 23, 9, 0, 0, 0, location), location)
	if err != nil {
		t.Fatalf("build dashboard: %v", err)
	}
	if response.CurrentMeeting.Status != "FREE" {
		t.Fatalf("expected room to be free, got %+v", response.CurrentMeeting)
	}
	if len(response.UpcomingMeetings) != 1 {
		t.Fatalf("expected one upcoming meeting, got %+v", response.UpcomingMeetings)
	}

	meeting := response.UpcomingMeetings[0]
	if meeting.StartTime != "10:00" || meeting.EndTime != "11:00" || meeting.Title != "Project review" || meeting.Owner != "Owner" {
		t.Fatalf("unexpected upcoming meeting: %+v", meeting)
	}
}

func TestBuildMeetingRoomDashboardUsesMeetingTitleWhenOrganizerNameIsUnavailable(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	response, err := buildMeetingRoomDashboard(meetingRoomScheduleResponse{MeetingRoomSchedules: []meetingRoomSchedules{{
		MeetingRoomID: "room-1",
		Schedules: []meetingSchedule{{
			EventID:   "event-1",
			Start:     meetingScheduleTime{DateTime: "2026-10-23T02:00:00Z"},
			End:       meetingScheduleTime{DateTime: "2026-10-23T03:00:00Z"},
			Summary:   "Project review",
			Organizer: meetingOrganizerRef{UserID: "booker-1"},
		}},
	}}}, "room-1", time.Date(2026, time.October, 23, 10, 30, 0, 0, location), location)
	if err != nil {
		t.Fatalf("build dashboard: %v", err)
	}
	if response.CurrentMeeting.Organizer != "Project review" {
		t.Fatalf("expected meeting title fallback, got %q", response.CurrentMeeting.Organizer)
	}
}

func TestDashboardRefreshInterval(t *testing.T) {
	interval, err := dashboardRefreshInterval("45s")
	if err != nil || interval != 45*time.Second {
		t.Fatalf("expected 45s interval, got %s, err=%v", interval, err)
	}
	interval, err = dashboardRefreshInterval("300")
	if err != nil || interval != 5*time.Minute {
		t.Fatalf("expected 300 seconds to equal 5m, got %s, err=%v", interval, err)
	}
	if _, err := dashboardRefreshInterval("0s"); err == nil {
		t.Fatal("expected zero interval to be rejected")
	}
}

func TestDashboardRefreshWindows(t *testing.T) {
	windows, err := dashboardRefreshWindows("08:00-12:00,17:30-18:00,22:00-02:00")
	if err != nil {
		t.Fatalf("parse refresh windows: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("expected three refresh windows, got %d", len(windows))
	}

	location := time.FixedZone("UTC+8", 8*60*60)
	testCases := []struct {
		name    string
		now     time.Time
		allowed bool
	}{
		{name: "at start", now: time.Date(2026, time.October, 27, 8, 0, 0, 0, location), allowed: true},
		{name: "within first window", now: time.Date(2026, time.October, 27, 11, 59, 0, 0, location), allowed: true},
		{name: "at end", now: time.Date(2026, time.October, 27, 12, 0, 0, 0, location), allowed: false},
		{name: "between windows", now: time.Date(2026, time.October, 27, 12, 30, 0, 0, location), allowed: false},
		{name: "within second window", now: time.Date(2026, time.October, 27, 17, 59, 0, 0, location), allowed: true},
		{name: "before second window", now: time.Date(2026, time.October, 27, 17, 6, 0, 0, location), allowed: false},
		{name: "within overnight window", now: time.Date(2026, time.October, 27, 23, 0, 0, 0, location), allowed: true},
		{name: "after midnight", now: time.Date(2026, time.October, 28, 1, 59, 0, 0, location), allowed: true},
		{name: "after overnight window", now: time.Date(2026, time.October, 28, 2, 0, 0, 0, location), allowed: false},
		{name: "saturday", now: time.Date(2026, time.October, 31, 9, 0, 0, 0, location), allowed: false},
		{name: "sunday", now: time.Date(2026, time.November, 1, 9, 0, 0, 0, location), allowed: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if allowed := dashboardRefreshAllowedAt(testCase.now, location, windows); allowed != testCase.allowed {
				t.Fatalf("expected allowed=%t, got %t", testCase.allowed, allowed)
			}
		})
	}

	if allowed := dashboardRefreshAllowedAt(time.Date(2026, time.October, 27, 9, 0, 0, 0, location), location, nil); !allowed {
		t.Fatal("expected no refresh windows to allow all weekday times")
	}
	for _, value := range []string{"08:00", "08:00-08:00", "25:00-26:00"} {
		if _, err := dashboardRefreshWindows(value); err == nil {
			t.Fatalf("expected invalid refresh windows %q to be rejected", value)
		}
	}
}
