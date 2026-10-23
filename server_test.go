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

func TestMeetingRoomScheduleForwardsRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1.0/oauth2/accessToken":
			body, _ := io.ReadAll(request.Body)
			if request.Method != http.MethodPost || string(body) != `{"appKey":"test-key","appSecret":"test-secret"}` {
				t.Errorf("unexpected token request: method=%s body=%s", request.Method, body)
			}
			_, _ = writer.Write([]byte(`{"accessToken":"test-token","expireIn":7200}`))
		case "/v1.0/calendar/users/user-1/meetingRooms/schedules/query":
			if got := request.Header.Get("x-acs-dingtalk-access-token"); got != "test-token" {
				t.Errorf("unexpected token: %q", got)
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"roomIds":["room-1"],"startTime":"2026-10-22T02:00:00Z","endTime":"2026-10-22T03:00:00Z"}` {
				t.Errorf("unexpected request body: %s", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"meetingRoomSchedules":[]}`))
		default:
			t.Errorf("unexpected path: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
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

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != `{"meetingRoomSchedules":[]}` {
		t.Fatalf("unexpected response: %s", response.Body.String())
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

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", response.Code, response.Body.String())
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
				{"roomId":"6a54b9f2d4b44318bfaeafe953d4914fb574d07771682d09","scheduleItems":[]},
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
	if !ready || len(schedules.schedules()) != len(meetingRoomIDByNumber) {
		t.Fatalf("unexpected cache contents: ready=%t schedules=%d", ready, len(schedules.schedules()))
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
	if response.CurrentMeeting.Status != "空闲" {
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
