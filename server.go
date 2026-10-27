package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDingTalkBaseURL          = "https://api.dingtalk.com"
	defaultDingTalkOAPIBaseURL      = "https://oapi.dingtalk.com"
	dingTalkResponseLimit           = 2 << 20
	tokenRefreshLead                = time.Minute
	defaultDashboardRefreshInterval = 300 * time.Second
)

var meetingRoomIDByNumber = map[string]string{
	"1": "6a54b9f2d4b44318bfaeafe953d4914fb574d07771682d09",
	"2": "202892463ff2314d533490c228e6847eb574d07771682d09",
	"3": "2d8e58ea71f66009492807e2949ba1edb574d07771682d09",
	"4": "5d8e21795c33aa2502bc497bb1db3c81b574d07771682d09",
	"5": "f8b00c2ed3e6ea2f36c8807a6ccbf605b574d07771682d09",
	"6": "88181f34d87dd7e67dbf33e867d3a59bb574d07771682d09",
}

type meetingRoomScheduleRequest struct {
	UserID    string   `json:"userId"`
	RoomIDs   []string `json:"roomIds"`
	StartTime string   `json:"startTime"`
	EndTime   string   `json:"endTime"`
}

type meetingRoomScheduleResponse struct {
	MeetingRoomSchedules []meetingRoomSchedules `json:"meetingRoomSchedules"`
	ScheduleInformation  []meetingRoomSchedules `json:"scheduleInformation"`
}

func (response meetingRoomScheduleResponse) schedules() []meetingRoomSchedules {
	if response.MeetingRoomSchedules != nil {
		return response.MeetingRoomSchedules
	}
	return response.ScheduleInformation
}

type meetingRoomSchedules struct {
	MeetingRoomID string            `json:"roomId"`
	Error         string            `json:"error"`
	Schedules     []meetingSchedule `json:"scheduleItems"`
}

type meetingSchedule struct {
	Start     meetingScheduleTime `json:"start"`
	End       meetingScheduleTime `json:"end"`
	EventID   string              `json:"eventId"`
	Status    string              `json:"status"`
	Summary   string              `json:"summary"`
	Title     string              `json:"title"`
	Organizer meetingOrganizerRef `json:"organizer"`
}

type meetingScheduleTime struct {
	DateTime string `json:"dateTime"`
}

type meetingOrganizerRef struct {
	UserID      string `json:"id"`
	DisplayName string `json:"displayName"`
}

type calendarEvent struct {
	ID        string              `json:"id"`
	Summary   string              `json:"summary"`
	Organizer meetingOrganizerRef `json:"organizer"`
}

type dashboardRoom struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Capacity int             `json:"capacity"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type dashboardCurrentMeeting struct {
	Status    string `json:"status"`
	Title     string `json:"title"`
	Organizer string `json:"organizer"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	EndAt     string `json:"endAt"`
}

type dashboardMeeting struct {
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Title     string `json:"title"`
	Owner     string `json:"owner"`
}

type meetingRoomDashboardResponse struct {
	Room             dashboardRoom           `json:"room"`
	CurrentMeeting   dashboardCurrentMeeting `json:"currentMeeting"`
	UpcomingMeetings []dashboardMeeting      `json:"upcomingMeetings"`
}

type meetingReservation struct {
	EventID   string
	Title     string
	Booker    meetingOrganizerRef
	StartTime string
	EndTime   string
}

type apiError struct {
	Error string `json:"error"`
}

type appAccessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireIn    int    `json:"expireIn"`
}

type dingTalkClient struct {
	appKey          string
	appSecret       string
	baseURL         string
	oapiBaseURL     string
	httpClient      *http.Client
	dashboardUserID string
	location        *time.Location
	tokenMu         sync.Mutex
	accessToken     string
	tokenExpiresAt  time.Time
	eventMu         sync.Mutex
	eventCache      map[string]calendarEvent
	organizerMu     sync.Mutex
	organizerNames  map[string]string
}

type dashboardScheduleCache struct {
	mu        sync.RWMutex
	schedules meetingRoomScheduleResponse
	updatedAt time.Time
	ready     bool
}

type dashboardRefreshWindow struct {
	startMinute int
	endMinute   int
}

func (cache *dashboardScheduleCache) store(schedules meetingRoomScheduleResponse, updatedAt time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	cache.schedules = schedules
	cache.updatedAt = updatedAt
	cache.ready = true
}

func (cache *dashboardScheduleCache) load() (meetingRoomScheduleResponse, time.Time, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	return cache.schedules, cache.updatedAt, cache.ready
}

func allMeetingRoomIDs() []string {
	roomNumbers := make([]int, 0, len(meetingRoomIDByNumber))
	for roomNumber := range meetingRoomIDByNumber {
		parsedRoomNumber, err := strconv.Atoi(roomNumber)
		if err != nil {
			continue
		}
		roomNumbers = append(roomNumbers, parsedRoomNumber)
	}
	sort.Ints(roomNumbers)

	roomIDs := make([]string, 0, len(roomNumbers))
	for _, roomNumber := range roomNumbers {
		roomIDs = append(roomIDs, meetingRoomIDByNumber[strconv.Itoa(roomNumber)])
	}
	return roomIDs
}

func (client *dingTalkClient) getMeetingRoomSchedule(
	ctx context.Context,
	request meetingRoomScheduleRequest,
) (json.RawMessage, int, error) {
	body, err := json.Marshal(struct {
		RoomIDs   []string `json:"roomIds"`
		StartTime string   `json:"startTime"`
		EndTime   string   `json:"endTime"`
	}{
		RoomIDs:   request.RoomIDs,
		StartTime: request.StartTime,
		EndTime:   request.EndTime,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("marshal DingTalk request: %w", err)
	}

	return client.callDingTalk(
		ctx,
		http.MethodPost,
		"/v1.0/calendar/users/"+url.PathEscape(request.UserID)+"/meetingRooms/schedules/query",
		body,
	)
}

func (client *dingTalkClient) getCalendarEvent(
	ctx context.Context,
	userID string,
	eventID string,
) (calendarEvent, error) {
	cacheKey := userID + ":" + eventID
	client.eventMu.Lock()
	cachedEvent, found := client.eventCache[cacheKey]
	client.eventMu.Unlock()
	if found {
		return cachedEvent, nil
	}

	responseBody, statusCode, err := client.callDingTalk(
		ctx,
		http.MethodGet,
		"/v1.0/calendar/users/"+url.PathEscape(userID)+
			"/calendars/primary/events/"+url.PathEscape(eventID),
		nil,
	)
	if err != nil {
		return calendarEvent{}, err
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return calendarEvent{}, dingTalkStatusError(statusCode, responseBody)
	}

	var event calendarEvent
	if err := json.Unmarshal(responseBody, &event); err != nil {
		return calendarEvent{}, fmt.Errorf("decode DingTalk calendar event: %w", err)
	}

	client.eventMu.Lock()
	if client.eventCache == nil {
		client.eventCache = make(map[string]calendarEvent)
	}
	client.eventCache[cacheKey] = event
	client.eventMu.Unlock()
	return event, nil
}

func (client *dingTalkClient) getOrganizerName(ctx context.Context, unionID string) (string, error) {
	client.organizerMu.Lock()
	name, found := client.organizerNames[unionID]
	client.organizerMu.Unlock()
	if found {
		return name, nil
	}

	accessToken, err := client.getAccessToken(ctx)
	if err != nil {
		return "", err
	}
	userIDResponse, err := client.callOAPI(ctx, "/topapi/user/getbyunionid", url.Values{
		"access_token": {accessToken},
		"unionid":      {unionID},
	})
	if err != nil {
		return "", err
	}
	var userIDResult struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Result  struct {
			UserID string `json:"userid"`
		} `json:"result"`
	}
	if err := json.Unmarshal(userIDResponse, &userIDResult); err != nil {
		return "", fmt.Errorf("decode DingTalk user ID response: %w", err)
	}
	if userIDResult.ErrCode != 0 || strings.TrimSpace(userIDResult.Result.UserID) == "" {
		return "", fmt.Errorf("resolve DingTalk user ID: %s", userIDResult.ErrMsg)
	}

	userResponse, err := client.callOAPI(ctx, "/topapi/v2/user/get", url.Values{
		"access_token": {accessToken},
		"userid":       {userIDResult.Result.UserID},
		"language":     {"zh_CN"},
	})
	if err != nil {
		return "", err
	}
	var userResult struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Result  struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(userResponse, &userResult); err != nil {
		return "", fmt.Errorf("decode DingTalk user response: %w", err)
	}
	name = strings.TrimSpace(userResult.Result.Name)
	if userResult.ErrCode != 0 || name == "" {
		return "", fmt.Errorf("read DingTalk user name: %s", userResult.ErrMsg)
	}

	client.organizerMu.Lock()
	if client.organizerNames == nil {
		client.organizerNames = make(map[string]string)
	}
	client.organizerNames[unionID] = name
	client.organizerMu.Unlock()
	return name, nil
}

func (client *dingTalkClient) getAccessToken(ctx context.Context) (string, error) {
	client.tokenMu.Lock()
	defer client.tokenMu.Unlock()

	if client.accessToken != "" && time.Now().Add(tokenRefreshLead).Before(client.tokenExpiresAt) {
		return client.accessToken, nil
	}

	body, err := json.Marshal(struct {
		AppKey    string `json:"appKey"`
		AppSecret string `json:"appSecret"`
	}{
		AppKey:    client.appKey,
		AppSecret: client.appSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal DingTalk token request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+"/v1.0/oauth2/accessToken",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("create DingTalk token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("request DingTalk access token: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, dingTalkResponseLimit))
	if err != nil {
		return "", fmt.Errorf("read DingTalk token response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", dingTalkStatusError(response.StatusCode, responseBody)
	}

	var tokenResponse appAccessTokenResponse
	if err := json.Unmarshal(responseBody, &tokenResponse); err != nil {
		return "", fmt.Errorf("decode DingTalk token response: %w", err)
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" {
		return "", errors.New("DingTalk token response did not include accessToken")
	}
	if tokenResponse.ExpireIn <= 0 {
		return "", errors.New("DingTalk token response did not include a valid expireIn")
	}

	client.accessToken = tokenResponse.AccessToken
	client.tokenExpiresAt = time.Now().Add(time.Duration(tokenResponse.ExpireIn) * time.Second)
	return client.accessToken, nil
}

func (client *dingTalkClient) callDingTalk(
	ctx context.Context,
	method string,
	path string,
	body []byte,
) (json.RawMessage, int, error) {
	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		method,
		client.baseURL+path,
		requestBody,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("create DingTalk request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	accessToken, err := client.getAccessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("x-acs-dingtalk-access-token", accessToken)

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("call DingTalk API: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, dingTalkResponseLimit))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read DingTalk response: %w", err)
	}
	return responseBody, response.StatusCode, nil
}

func (client *dingTalkClient) callOAPI(ctx context.Context, path string, form url.Values) ([]byte, error) {
	baseURL := client.oapiBaseURL
	if baseURL == "" {
		baseURL = defaultDingTalkOAPIBaseURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create DingTalk OAPI request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call DingTalk OAPI: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, dingTalkResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read DingTalk OAPI response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, dingTalkStatusError(response.StatusCode, responseBody)
	}
	return responseBody, nil
}

func newHTTPHandler(client *dingTalkClient, cache *dashboardScheduleCache) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /v1/meeting-rooms/schedules", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusGone, apiError{Error: "direct DingTalk schedule queries are disabled; use the cached dashboard status endpoint"})
	})

	mux.HandleFunc("GET /api/dingtalk/meeting-rooms/{roomId}/status", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Access-Control-Allow-Origin", "*")

		if client == nil {
			writeJSON(writer, http.StatusInternalServerError, apiError{Error: "DingTalk client is not configured"})
			return
		}

		roomNumber := strings.TrimSpace(request.PathValue("roomId"))
		roomID, found := resolveMeetingRoomID(roomNumber)
		if !found {
			writeJSON(writer, http.StatusBadRequest, apiError{Error: "roomId must be a number between 1 and 6"})
			return
		}
		if strings.TrimSpace(client.dashboardUserID) == "" {
			writeJSON(writer, http.StatusInternalServerError, apiError{Error: "DINGTALK_USER_ID is not configured"})
			return
		}

		if cache == nil {
			writeJSON(writer, http.StatusInternalServerError, apiError{Error: "dashboard cache is not configured"})
			return
		}

		schedules, _, ready := cache.load()
		if !ready {
			writeJSON(writer, http.StatusServiceUnavailable, apiError{Error: "dashboard schedule cache is not ready"})
			return
		}

		response, err := buildMeetingRoomDashboard(schedules, roomID, time.Now().In(client.location), client.location)
		if err != nil {
			log.Printf("build cached dashboard response failed for room %s: %v", roomID, err)
			writeJSON(writer, http.StatusInternalServerError, apiError{Error: "dashboard cache data is invalid"})
			return
		}

		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, response)
	})

	return mux
}

func resolveMeetingRoomID(roomNumber string) (string, bool) {
	roomID, found := meetingRoomIDByNumber[strings.TrimSpace(roomNumber)]
	return roomID, found
}

func refreshDashboardScheduleCache(ctx context.Context, client *dingTalkClient, cache *dashboardScheduleCache, now time.Time) error {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, client.location)
	rangeEnd := todayStart.AddDate(0, 0, 1)

	responseBody, statusCode, err := client.getMeetingRoomSchedule(ctx, meetingRoomScheduleRequest{
		UserID:    client.dashboardUserID,
		RoomIDs:   allMeetingRoomIDs(),
		StartTime: formatDingTalkTimestamp(todayStart),
		EndTime:   formatDingTalkTimestamp(rangeEnd),
	})
	if err != nil {
		return err
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return dingTalkStatusError(statusCode, responseBody)
	}

	var schedules meetingRoomScheduleResponse
	if err := json.Unmarshal(responseBody, &schedules); err != nil {
		return fmt.Errorf("decode DingTalk meeting room schedules: %w", err)
	}
	populateScheduleMetadata(ctx, client, &schedules)

	cache.store(schedules, now)
	return nil
}

func populateScheduleMetadata(ctx context.Context, client *dingTalkClient, response *meetingRoomScheduleResponse) {
	for scheduleIndex := range response.MeetingRoomSchedules {
		populateRoomScheduleMetadata(ctx, client, &response.MeetingRoomSchedules[scheduleIndex])
	}
	for scheduleIndex := range response.ScheduleInformation {
		populateRoomScheduleMetadata(ctx, client, &response.ScheduleInformation[scheduleIndex])
	}
}

func populateRoomScheduleMetadata(ctx context.Context, client *dingTalkClient, roomSchedule *meetingRoomSchedules) {
	for scheduleIndex := range roomSchedule.Schedules {
		schedule := &roomSchedule.Schedules[scheduleIndex]
		if strings.TrimSpace(schedule.Organizer.UserID) == "" || strings.TrimSpace(schedule.EventID) == "" {
			continue
		}

		name, err := client.getOrganizerName(ctx, schedule.Organizer.UserID)
		if err != nil {
			log.Printf("DingTalk organizer name request failed for organizer %s: %v", schedule.Organizer.UserID, err)
		} else {
			schedule.Organizer.DisplayName = name
		}

		event, err := client.getCalendarEvent(ctx, schedule.Organizer.UserID, schedule.EventID)
		if err != nil {
			log.Printf("DingTalk event detail request failed for event %s: %v", schedule.EventID, err)
			continue
		}
		if strings.TrimSpace(event.Summary) != "" {
			schedule.Summary = event.Summary
		}
	}
}

func refreshDashboardScheduleCachePeriodically(
	ctx context.Context,
	client *dingTalkClient,
	cache *dashboardScheduleCache,
	interval time.Duration,
	refreshWindows []dashboardRefreshWindow,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !dashboardRefreshAllowedAt(now, client.location, refreshWindows) {
				continue
			}
			if err := refreshDashboardScheduleCache(ctx, client, cache, now.In(client.location)); err != nil {
				log.Printf("refresh dashboard schedule cache: %v", err)
			}
		}
	}
}

func buildMeetingRoomDashboard(
	schedulesResponse meetingRoomScheduleResponse,
	roomID string,
	now time.Time,
	location *time.Location,
) (meetingRoomDashboardResponse, error) {
	reservations, err := reservationsForRoom(schedulesResponse, roomID)
	if err != nil {
		return meetingRoomDashboardResponse{}, err
	}

	response := meetingRoomDashboardResponse{
		Room: dashboardRoom{
			ID: roomID,
		},
		CurrentMeeting: dashboardCurrentMeeting{
			Status:    "FREE",
			Title:     "There are no meetings at present",
			Organizer: "—",
			StartTime: "—",
			EndTime:   "—",
			EndAt:     "",
		},
		UpcomingMeetings: make([]dashboardMeeting, 0),
	}
	for _, reservation := range reservations {
		startTime, err := parseDingTalkTimestamp(reservation.StartTime)
		if err != nil {
			return meetingRoomDashboardResponse{}, err
		}
		endTime, err := parseDingTalkTimestamp(reservation.EndTime)
		if err != nil {
			return meetingRoomDashboardResponse{}, err
		}

		meeting := dashboardMeeting{
			StartTime: startTime.In(location).Format("15:04"),
			EndTime:   endTime.In(location).Format("15:04"),
			Title:     reservation.Title,
			Owner:     reservation.Booker.DisplayName,
		}

		if !now.Before(startTime) && now.Before(endTime) {
			response.CurrentMeeting = dashboardCurrentMeeting{
				Status:    "BUSY",
				Title:     reservation.Title,
				Organizer: reservation.Booker.DisplayName,
				StartTime: meeting.StartTime,
				EndTime:   meeting.EndTime,
				EndAt:     formatTimestamp(endTime),
			}
		}

		if startTime.After(now) && isSameDay(startTime, now, location) {
			response.UpcomingMeetings = append(response.UpcomingMeetings, meeting)
		}
	}

	return response, nil
}

func reservationsForRoom(
	response meetingRoomScheduleResponse,
	roomID string,
) ([]meetingReservation, error) {
	reservations := make([]meetingReservation, 0)

	for _, roomSchedule := range response.schedules() {
		if roomSchedule.MeetingRoomID != roomID {
			continue
		}
		if strings.TrimSpace(roomSchedule.Error) != "" {
			return nil, fmt.Errorf("DingTalk returned an error for room %s: %s", roomID, roomSchedule.Error)
		}

		for _, schedule := range roomSchedule.Schedules {
			startTime, err := parseDingTalkTimestamp(schedule.Start.DateTime)
			if err != nil {
				return nil, fmt.Errorf("parse schedule start time: %w", err)
			}
			endTime, err := parseDingTalkTimestamp(schedule.End.DateTime)
			if err != nil {
				return nil, fmt.Errorf("parse schedule end time: %w", err)
			}
			if !endTime.After(startTime) {
				return nil, errors.New("DingTalk returned a schedule with an invalid time range")
			}
			if strings.TrimSpace(schedule.EventID) == "" {
				return nil, errors.New("DingTalk returned a schedule without eventId")
			}
			booker := schedule.Organizer
			if strings.TrimSpace(booker.DisplayName) == "" {
				booker.DisplayName = booker.UserID
			}

			title := firstNonEmpty(schedule.Summary, schedule.Title)
			if title == "" {
				title = "Scheduled meeting"
			}

			reservations = append(reservations, meetingReservation{
				EventID:   schedule.EventID,
				Title:     title,
				Booker:    booker,
				StartTime: formatTimestamp(startTime),
				EndTime:   formatTimestamp(endTime),
			})
		}
	}

	sort.Slice(reservations, func(first int, second int) bool {
		if reservations[first].StartTime == reservations[second].StartTime {
			return reservations[first].EventID < reservations[second].EventID
		}
		return reservations[first].StartTime < reservations[second].StartTime
	})

	return reservations, nil
}

func normalizeScheduleRequest(request meetingRoomScheduleRequest) (meetingRoomScheduleRequest, error) {
	if strings.TrimSpace(request.UserID) == "" {
		return meetingRoomScheduleRequest{}, errors.New("userId is required")
	}
	if len(request.RoomIDs) == 0 || len(request.RoomIDs) > len(meetingRoomIDByNumber) {
		return meetingRoomScheduleRequest{}, fmt.Errorf("roomIds must contain between 1 and %d items", len(meetingRoomIDByNumber))
	}
	for _, roomID := range request.RoomIDs {
		if strings.TrimSpace(roomID) == "" {
			return meetingRoomScheduleRequest{}, errors.New("roomIds must not contain empty values")
		}
	}

	startTime, err := time.Parse(time.RFC3339, request.StartTime)
	if err != nil {
		return meetingRoomScheduleRequest{}, errors.New("startTime must be an ISO 8601 timestamp")
	}
	endTime, err := time.Parse(time.RFC3339, request.EndTime)
	if err != nil {
		return meetingRoomScheduleRequest{}, errors.New("endTime must be an ISO 8601 timestamp")
	}
	if !endTime.After(startTime) {
		return meetingRoomScheduleRequest{}, errors.New("endTime must be after startTime")
	}

	request.StartTime = formatDingTalkTimestamp(startTime)
	request.EndTime = formatDingTalkTimestamp(endTime)
	return request, nil
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		return errors.New("request body must be valid JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func parseDingTalkTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid DingTalk timestamp %q: %w", value, err)
	}
	return parsed.UTC(), nil
}

func formatTimestamp(value time.Time) string {
	return value.Format(time.RFC3339)
}

func formatDingTalkTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05Z")
}

func isSameDay(first time.Time, second time.Time, location *time.Location) bool {
	first = first.In(location)
	second = second.In(location)

	return first.Year() == second.Year() &&
		first.Month() == second.Month() &&
		first.Day() == second.Day()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func dingTalkStatusError(statusCode int, responseBody []byte) error {
	message := strings.TrimSpace(string(responseBody))
	if message == "" {
		return fmt.Errorf("DingTalk API returned HTTP %d", statusCode)
	}
	return fmt.Errorf("DingTalk API returned HTTP %d: %s", statusCode, message)
}

func dashboardRefreshInterval(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultDashboardRefreshInterval, nil
	}

	interval, err := time.ParseDuration(value)
	if err != nil {
		seconds, parseSecondsErr := strconv.ParseInt(value, 10, 64)
		if parseSecondsErr == nil {
			interval = time.Duration(seconds) * time.Second
			err = nil
		}
	}
	if err != nil {
		return 0, fmt.Errorf("parse DASHBOARD_REFRESH_INTERVAL: use a Go duration such as 30s or a whole number of seconds: %w", err)
	}
	if interval <= 0 {
		return 0, errors.New("DASHBOARD_REFRESH_INTERVAL must be greater than zero")
	}
	return interval, nil
}

func dashboardRefreshWindows(value string) ([]dashboardRefreshWindow, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	windows := make([]dashboardRefreshWindow, 0, len(strings.Split(value, ",")))
	for _, windowValue := range strings.Split(value, ",") {
		windowValue = strings.TrimSpace(windowValue)
		start, end, found := strings.Cut(windowValue, "-")
		if !found || strings.Contains(end, "-") {
			return nil, fmt.Errorf("parse DASHBOARD_REFRESH_WINDOWS: %q must use HH:MM-HH:MM", windowValue)
		}

		startMinute, err := dashboardRefreshMinute(start)
		if err != nil {
			return nil, err
		}
		endMinute, err := dashboardRefreshMinute(end)
		if err != nil {
			return nil, err
		}
		if startMinute == endMinute {
			return nil, fmt.Errorf("parse DASHBOARD_REFRESH_WINDOWS: %q must not have identical start and end times", windowValue)
		}

		windows = append(windows, dashboardRefreshWindow{startMinute: startMinute, endMinute: endMinute})
	}
	return windows, nil
}

func dashboardRefreshMinute(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("parse DASHBOARD_REFRESH_WINDOWS: %q must use HH:MM", strings.TrimSpace(value))
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func dashboardRefreshAllowedAt(now time.Time, location *time.Location, windows []dashboardRefreshWindow) bool {
	localNow := now.In(location)
	if localNow.Weekday() == time.Saturday || localNow.Weekday() == time.Sunday {
		return false
	}
	if len(windows) == 0 {
		return true
	}

	minute := localNow.Hour()*60 + localNow.Minute()
	for _, window := range windows {
		if window.startMinute < window.endMinute && minute >= window.startMinute && minute < window.endMinute {
			return true
		}
		if window.startMinute > window.endMinute && (minute >= window.startMinute || minute < window.endMinute) {
			return true
		}
	}
	return false
}

func writeJSON(writer http.ResponseWriter, statusCode int, response any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	_ = json.NewEncoder(writer).Encode(response)
}

func main() {
	appKey := os.Getenv("DINGTALK_APP_KEY")
	if appKey == "" {
		log.Fatal("DINGTALK_APP_KEY is required")
	}
	appSecret := os.Getenv("DINGTALK_APP_SECRET")
	if appSecret == "" {
		log.Fatal("DINGTALK_APP_SECRET is required")
	}

	dashboardUserID := os.Getenv("DINGTALK_USER_ID")
	if dashboardUserID == "" {
		log.Fatal("DINGTALK_USER_ID is required")
	}

	baseURL := os.Getenv("DINGTALK_BASE_URL")
	if baseURL == "" {
		baseURL = defaultDingTalkBaseURL
	}

	address := os.Getenv("LISTEN_ADDR")
	if address == "" {
		address = ":8080"
	}

	locationName := os.Getenv("DASHBOARD_TIME_ZONE")
	if locationName == "" {
		locationName = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(locationName)
	if err != nil {
		log.Fatalf("invalid DASHBOARD_TIME_ZONE: %v", err)
	}
	refreshInterval, err := dashboardRefreshInterval(os.Getenv("DASHBOARD_REFRESH_INTERVAL"))
	if err != nil {
		log.Fatal(err)
	}
	refreshWindows, err := dashboardRefreshWindows(os.Getenv("DASHBOARD_REFRESH_WINDOWS"))
	if err != nil {
		log.Fatal(err)
	}

	client := &dingTalkClient{
		appKey:          appKey,
		appSecret:       appSecret,
		baseURL:         strings.TrimRight(baseURL, "/"),
		httpClient:      &http.Client{Timeout: 15 * time.Second},
		dashboardUserID: dashboardUserID,
		location:        location,
	}
	cache := &dashboardScheduleCache{}
	now := time.Now().In(location)
	if dashboardRefreshAllowedAt(now, location, refreshWindows) {
		if err := refreshDashboardScheduleCache(context.Background(), client, cache, now); err != nil {
			log.Printf("initial dashboard schedule cache refresh failed: %v", err)
		}
	} else {
		log.Printf("skip initial dashboard schedule cache refresh outside workday or refresh window")
	}
	go refreshDashboardScheduleCachePeriodically(context.Background(), client, cache, refreshInterval, refreshWindows)

	log.Printf("meeting room schedule API listening on %s; dashboard cache refresh interval: %s; refresh windows: %s", address, refreshInterval, firstNonEmpty(os.Getenv("DASHBOARD_REFRESH_WINDOWS"), "workdays all day"))
	log.Fatal(http.ListenAndServe(address, newHTTPHandler(client, cache)))
}
