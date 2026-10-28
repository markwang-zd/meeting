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
	defaultDingTalkContactBaseURL   = "https://oapi.dingtalk.com"
	dingTalkResponseLimit           = 2 << 20
	tokenRefreshLead                = time.Minute
	defaultDashboardRefreshInterval = 300 * time.Second
	organizerCacheRefreshHour       = 8
	organizerDirectoryPageSize      = 100
	meetingRoomCacheRefreshHour     = 8
	meetingRoomListPageSize         = 100
)

type meetingRoomMetadata struct {
	DingTalkID string
	Name       string
	Capacity   int
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

type dashboardRoom struct {
	ID       string          `json:"id"`
	Number   string          `json:"number"`
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

type dingTalkDirectoryUser struct {
	Name    string `json:"name"`
	UnionID string `json:"unionid"`
}

type dingTalkMeetingRoomListResponse struct {
	HasMore bool                  `json:"hasMore"`
	Rooms   []dingTalkMeetingRoom `json:"result"`
}

type dingTalkMeetingRoom struct {
	ID       string `json:"roomId"`
	Name     string `json:"roomName"`
	Capacity int    `json:"roomCapacity"`
}

type dingTalkDepartmentUsersResponse struct {
	ErrorCode    int                     `json:"errcode"`
	ErrorMessage string                  `json:"errmsg"`
	Result       dingTalkDepartmentUsers `json:"result"`
}

type dingTalkDepartmentUsers struct {
	HasMore    bool                    `json:"has_more"`
	Users      []dingTalkDirectoryUser `json:"list"`
	NextCursor int                     `json:"next_cursor"`
}

type dingTalkClient struct {
	appKey          string
	appSecret       string
	baseURL         string
	contactBaseURL  string
	httpClient      *http.Client
	dashboardUserID string
	location        *time.Location
	tokenMu         sync.Mutex
	accessToken     string
	tokenExpiresAt  time.Time
}

type organizerNameCache struct {
	mu        sync.RWMutex
	names     map[string]string
	updatedAt time.Time
}

type dashboardScheduleCache struct {
	mu        sync.RWMutex
	schedules meetingRoomScheduleResponse
	updatedAt time.Time
	ready     bool
}

type meetingRoomCache struct {
	mu        sync.RWMutex
	byNumber  map[string]meetingRoomMetadata
	byID      map[string]meetingRoomMetadata
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

func (cache *meetingRoomCache) store(rooms []dingTalkMeetingRoom, updatedAt time.Time) error {
	metadata := make([]meetingRoomMetadata, 0, len(rooms))
	for _, room := range rooms {
		roomID := strings.TrimSpace(room.ID)
		if roomID == "" {
			return errors.New("DingTalk returned a meeting room without roomId")
		}
		metadata = append(metadata, meetingRoomMetadata{
			DingTalkID: roomID,
			Name:       strings.TrimSpace(room.Name),
			Capacity:   room.Capacity,
		})
	}
	sort.Slice(metadata, func(first int, second int) bool {
		return metadata[first].DingTalkID < metadata[second].DingTalkID
	})

	byNumber := make(map[string]meetingRoomMetadata, len(metadata))
	byID := make(map[string]meetingRoomMetadata, len(metadata))
	for index, room := range metadata {
		if _, duplicate := byID[room.DingTalkID]; duplicate {
			return fmt.Errorf("DingTalk returned duplicate meeting room ID %q", room.DingTalkID)
		}
		byNumber[strconv.Itoa(index+1)] = room
		byID[room.DingTalkID] = room
	}
	if len(byNumber) == 0 {
		return errors.New("DingTalk returned no meeting rooms")
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.byNumber = byNumber
	cache.byID = byID
	cache.updatedAt = updatedAt
	cache.ready = true
	return nil
}

func (cache *meetingRoomCache) roomID(number string) (string, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	metadata, found := cache.byNumber[strings.TrimSpace(number)]
	return metadata.DingTalkID, found
}

func (cache *meetingRoomCache) roomMetadata(roomID string) (meetingRoomMetadata, string, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	metadata, found := cache.byID[roomID]
	if !found {
		return meetingRoomMetadata{}, "", false
	}
	for number, candidate := range cache.byNumber {
		if candidate.DingTalkID == roomID {
			return metadata, number, true
		}
	}
	return meetingRoomMetadata{}, "", false
}

func (cache *meetingRoomCache) roomIDs() ([]string, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if !cache.ready {
		return nil, false
	}

	roomNumbers := make([]int, 0, len(cache.byNumber))
	for roomNumber := range cache.byNumber {
		parsedRoomNumber, err := strconv.Atoi(roomNumber)
		if err == nil {
			roomNumbers = append(roomNumbers, parsedRoomNumber)
		}
	}
	sort.Ints(roomNumbers)
	roomIDs := make([]string, 0, len(roomNumbers))
	for _, roomNumber := range roomNumbers {
		roomIDs = append(roomIDs, cache.byNumber[strconv.Itoa(roomNumber)].DingTalkID)
	}
	return roomIDs, true
}

func (cache *meetingRoomCache) refreshDue(now time.Time, location *time.Location) bool {
	cache.mu.RLock()
	updatedAt := cache.updatedAt
	ready := cache.ready
	cache.mu.RUnlock()

	localNow := now.In(location)
	return !ready || (localNow.Hour() >= meetingRoomCacheRefreshHour && !isSameDay(updatedAt, localNow, location))
}

func newOrganizerNameCache() *organizerNameCache {
	return &organizerNameCache{names: make(map[string]string)}
}

func (cache *organizerNameCache) store(names map[string]string, updatedAt time.Time) {
	normalizedNames := normalizeOrganizerNames(names)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.names = normalizedNames
	cache.updatedAt = updatedAt
}

func (cache *organizerNameCache) namesSnapshot() map[string]string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	names := make(map[string]string, len(cache.names))
	for userID, name := range cache.names {
		names[userID] = name
	}
	return names
}

func (cache *organizerNameCache) refreshDue(now time.Time, location *time.Location) bool {
	cache.mu.RLock()
	updatedAt := cache.updatedAt
	cache.mu.RUnlock()

	localNow := now.In(location)
	return localNow.Hour() >= organizerCacheRefreshHour && !isSameDay(updatedAt, localNow, location)
}

func normalizeOrganizerNames(names map[string]string) map[string]string {
	normalizedNames := make(map[string]string, len(names))
	for userID, name := range names {
		if userID = strings.TrimSpace(userID); userID != "" {
			if name = strings.TrimSpace(name); name != "" {
				normalizedNames[userID] = name
			}
		}
	}
	return normalizedNames
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

func (client *dingTalkClient) getMeetingRooms(ctx context.Context) ([]dingTalkMeetingRoom, error) {
	requestURL, err := url.Parse(client.baseURL + "/v1.0/rooms/meetingRoomLists")
	if err != nil {
		return nil, fmt.Errorf("parse DingTalk meeting rooms URL: %w", err)
	}
	query := requestURL.Query()
	query.Set("maxResults", strconv.Itoa(meetingRoomListPageSize))
	query.Set("unionId", client.dashboardUserID)
	requestURL.RawQuery = query.Encode()

	accessToken, err := client.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create DingTalk meeting rooms request: %w", err)
	}
	request.Header.Set("x-acs-dingtalk-access-token", accessToken)

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call DingTalk meeting rooms API: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, dingTalkResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read DingTalk meeting rooms response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, dingTalkStatusError(response.StatusCode, responseBody)
	}

	var roomsResponse dingTalkMeetingRoomListResponse
	if err := json.Unmarshal(responseBody, &roomsResponse); err != nil {
		return nil, fmt.Errorf("decode DingTalk meeting rooms: %w", err)
	}
	if roomsResponse.HasMore {
		return nil, fmt.Errorf("DingTalk returned more than %d meeting rooms; refusing an incomplete cache to preserve the one-request limit", meetingRoomListPageSize)
	}
	return roomsResponse.Rooms, nil
}

func (client *dingTalkClient) listDepartmentUsers(ctx context.Context, departmentID int64, cursor int) (dingTalkDepartmentUsers, error) {
	body, err := json.Marshal(struct {
		DepartmentID       int64 `json:"dept_id"`
		Cursor             int   `json:"cursor"`
		Size               int   `json:"size"`
		ContainAccessLimit bool  `json:"contain_access_limit"`
	}{
		DepartmentID:       departmentID,
		Cursor:             cursor,
		Size:               organizerDirectoryPageSize,
		ContainAccessLimit: false,
	})
	if err != nil {
		return dingTalkDepartmentUsers{}, fmt.Errorf("marshal DingTalk department users request: %w", err)
	}

	responseBody, statusCode, err := client.callDingTalkContact(ctx, "/topapi/v2/user/list", body)
	if err != nil {
		return dingTalkDepartmentUsers{}, err
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return dingTalkDepartmentUsers{}, dingTalkStatusError(statusCode, responseBody)
	}

	var response dingTalkDepartmentUsersResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return dingTalkDepartmentUsers{}, fmt.Errorf("decode DingTalk department users: %w", err)
	}
	if response.ErrorCode != 0 {
		return dingTalkDepartmentUsers{}, fmt.Errorf("DingTalk department users API returned errcode %d: %s", response.ErrorCode, response.ErrorMessage)
	}
	return response.Result, nil
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

func (client *dingTalkClient) callDingTalkContact(
	ctx context.Context,
	path string,
	body []byte,
) (json.RawMessage, int, error) {
	accessToken, err := client.getAccessToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	requestURL, err := url.Parse(client.contactBaseURL + path)
	if err != nil {
		return nil, 0, fmt.Errorf("parse DingTalk contact API URL: %w", err)
	}
	query := requestURL.Query()
	query.Set("access_token", accessToken)
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create DingTalk contact API request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("call DingTalk contact API: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, dingTalkResponseLimit))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read DingTalk contact API response: %w", err)
	}
	return responseBody, response.StatusCode, nil
}

func newHTTPHandler(client *dingTalkClient, cache *dashboardScheduleCache, organizerNames *organizerNameCache, rooms *meetingRoomCache) http.Handler {
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

		if rooms == nil {
			writeJSON(writer, http.StatusInternalServerError, apiError{Error: "meeting room cache is not configured"})
			return
		}

		roomNumber := strings.TrimSpace(request.PathValue("roomId"))
		roomID, found := rooms.roomID(roomNumber)
		if !found {
			writeJSON(writer, http.StatusBadRequest, apiError{Error: "roomId does not exist in the meeting room cache"})
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

		var names map[string]string
		if organizerNames != nil {
			names = organizerNames.namesSnapshot()
		}
		response, err := buildMeetingRoomDashboard(schedules, roomID, time.Now().In(client.location), client.location, names, rooms)
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

func refreshMeetingRoomCache(ctx context.Context, client *dingTalkClient, cache *meetingRoomCache, now time.Time) error {
	rooms, err := client.getMeetingRooms(ctx)
	if err != nil {
		return err
	}
	return cache.store(rooms, now)
}

func refreshMeetingRoomCachePeriodically(ctx context.Context, client *dingTalkClient, cache *meetingRoomCache) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !cache.refreshDue(now, client.location) {
				continue
			}
			if err := refreshMeetingRoomCache(ctx, client, cache, now.In(client.location)); err != nil {
				log.Printf("refresh meeting room cache: %v", err)
			}
		}
	}
}

func refreshDashboardScheduleCache(ctx context.Context, client *dingTalkClient, cache *dashboardScheduleCache, rooms *meetingRoomCache, now time.Time) error {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, client.location)
	rangeEnd := todayStart.AddDate(0, 0, 1)
	roomIDs, ready := rooms.roomIDs()
	if !ready {
		return errors.New("meeting room cache is not ready")
	}

	responseBody, statusCode, err := client.getMeetingRoomSchedule(ctx, meetingRoomScheduleRequest{
		UserID:    client.dashboardUserID,
		RoomIDs:   roomIDs,
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
	cache.store(schedules, now)
	return nil
}

func refreshDashboardScheduleCachePeriodically(
	ctx context.Context,
	client *dingTalkClient,
	cache *dashboardScheduleCache,
	rooms *meetingRoomCache,
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
			if err := refreshDashboardScheduleCache(ctx, client, cache, rooms, now.In(client.location)); err != nil {
				log.Printf("refresh dashboard schedule cache: %v", err)
			}
		}
	}
}

func refreshOrganizerNameCache(ctx context.Context, client *dingTalkClient, cache *organizerNameCache, now time.Time) error {
	page, err := client.listDepartmentUsers(ctx, 1, 0)
	if err != nil {
		return fmt.Errorf("list users for DingTalk root department: %w", err)
	}
	if page.HasMore {
		return fmt.Errorf("DingTalk root department returned more than %d users; refusing an incomplete organizer cache to preserve the one-request limit", organizerDirectoryPageSize)
	}

	names := make(map[string]string, len(page.Users))
	for _, user := range page.Users {
		if unionID, name := strings.TrimSpace(user.UnionID), strings.TrimSpace(user.Name); unionID != "" && name != "" {
			names[unionID] = name
		}
	}

	cache.store(names, now)
	return nil
}

func refreshOrganizerNameCachePeriodically(ctx context.Context, client *dingTalkClient, cache *organizerNameCache) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !cache.refreshDue(now, client.location) {
				continue
			}
			if err := refreshOrganizerNameCache(ctx, client, cache, now.In(client.location)); err != nil {
				log.Printf("refresh organizer name cache: %v", err)
			}
		}
	}
}

func buildMeetingRoomDashboard(
	schedulesResponse meetingRoomScheduleResponse,
	roomID string,
	now time.Time,
	location *time.Location,
	organizerNames map[string]string,
	rooms *meetingRoomCache,
) (meetingRoomDashboardResponse, error) {
	reservations, err := reservationsForRoom(schedulesResponse, roomID, organizerNames)
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
	if metadata, roomNumber, found := rooms.roomMetadata(roomID); found {
		response.Room.Number = roomNumber
		response.Room.Name = metadata.Name
		response.Room.Capacity = metadata.Capacity
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
	organizerNames map[string]string,
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
			title := firstNonEmpty(schedule.Summary, schedule.Title)
			if title == "" {
				title = "Reserved"
			}
			booker := schedule.Organizer
			if strings.TrimSpace(booker.DisplayName) == "" {
				booker.DisplayName = organizerNames[strings.TrimSpace(booker.UserID)]
			}
			if strings.TrimSpace(booker.DisplayName) == "" {
				booker.DisplayName = "—"
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
	if len(request.RoomIDs) == 0 || len(request.RoomIDs) > meetingRoomListPageSize {
		return meetingRoomScheduleRequest{}, fmt.Errorf("roomIds must contain between 1 and %d items", meetingRoomListPageSize)
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
	contactBaseURL := os.Getenv("DINGTALK_CONTACT_BASE_URL")
	if contactBaseURL == "" {
		contactBaseURL = defaultDingTalkContactBaseURL
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
		contactBaseURL:  strings.TrimRight(contactBaseURL, "/"),
		httpClient:      &http.Client{Timeout: 15 * time.Second},
		dashboardUserID: dashboardUserID,
		location:        location,
	}
	cache := &dashboardScheduleCache{}
	meetingRooms := &meetingRoomCache{}
	organizerNames := newOrganizerNameCache()
	now := time.Now().In(location)
	if meetingRooms.refreshDue(now, location) {
		if err := refreshMeetingRoomCache(context.Background(), client, meetingRooms, now); err != nil {
			log.Printf("initial meeting room cache refresh failed: %v", err)
		}
	}
	if organizerNames.refreshDue(now, location) {
		if err := refreshOrganizerNameCache(context.Background(), client, organizerNames, now); err != nil {
			log.Printf("initial organizer name cache refresh failed: %v", err)
		}
	}
	if dashboardRefreshAllowedAt(now, location, refreshWindows) {
		if err := refreshDashboardScheduleCache(context.Background(), client, cache, meetingRooms, now); err != nil {
			log.Printf("initial dashboard schedule cache refresh failed: %v", err)
		}
	} else {
		log.Printf("skip initial dashboard schedule cache refresh outside workday or refresh window")
	}
	go refreshDashboardScheduleCachePeriodically(context.Background(), client, cache, meetingRooms, refreshInterval, refreshWindows)
	go refreshOrganizerNameCachePeriodically(context.Background(), client, organizerNames)
	go refreshMeetingRoomCachePeriodically(context.Background(), client, meetingRooms)

	log.Printf("meeting room schedule API listening on %s; dashboard cache refresh interval: %s; refresh windows: %s", address, refreshInterval, firstNonEmpty(os.Getenv("DASHBOARD_REFRESH_WINDOWS"), "workdays all day"))
	log.Fatal(http.ListenAndServe(address, newHTTPHandler(client, cache, organizerNames, meetingRooms)))
}
