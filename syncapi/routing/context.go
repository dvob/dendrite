// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	"github.com/matrix-org/dendrite/internal/caching"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	roomserver "github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/syncapi/internal"
	"github.com/matrix-org/dendrite/syncapi/storage"
	"github.com/matrix-org/dendrite/syncapi/types"
	userapi "github.com/matrix-org/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
)

type ContextRespsonse struct {
	End          string                          `json:"end"`
	Event        *gomatrixserverlib.ClientEvent  `json:"event,omitempty"`
	EventsAfter  []gomatrixserverlib.ClientEvent `json:"events_after,omitempty"`
	EventsBefore []gomatrixserverlib.ClientEvent `json:"events_before,omitempty"`
	Start        string                          `json:"start"`
	State        []gomatrixserverlib.ClientEvent `json:"state,omitempty"`
}

func Context(
	req *http.Request, device *userapi.Device,
	rsAPI roomserver.SyncRoomserverAPI,
	syncDB storage.Database,
	roomID, eventID string,
	lazyLoadCache caching.LazyLoadCache,
) util.JSONResponse {
	snapshot, err := syncDB.NewDatabaseSnapshot(req.Context())
	if err != nil {
		return jsonerror.InternalServerError()
	}
	var succeeded bool
	defer sqlutil.EndTransactionWithCheck(snapshot, &succeeded, &err)

	filter, err := parseRoomEventFilter(req)
	if err != nil {
		errMsg := ""
		switch err.(type) {
		case *json.InvalidUnmarshalError:
			errMsg = "unable to parse filter"
		case *strconv.NumError:
			errMsg = "unable to parse limit"
		default:
			errMsg = err.Error()
		}
		return util.JSONResponse{
			Code:    http.StatusBadRequest,
			JSON:    jsonerror.InvalidParam(errMsg),
			Headers: nil,
		}
	}
	if filter.Rooms != nil {
		*filter.Rooms = append(*filter.Rooms, roomID)
	}

	ctx := req.Context()
	membershipRes := roomserver.QueryMembershipForUserResponse{}
	membershipReq := roomserver.QueryMembershipForUserRequest{UserID: device.UserID, RoomID: roomID}
	if err = rsAPI.QueryMembershipForUser(ctx, &membershipReq, &membershipRes); err != nil {
		logrus.WithError(err).Error("unable to query membership")
		return jsonerror.InternalServerError()
	}
	if !membershipRes.RoomExists {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: jsonerror.Forbidden("room does not exist"),
		}
	}

	stateFilter := gomatrixserverlib.StateFilter{
		NotSenders:              filter.NotSenders,
		NotTypes:                filter.NotTypes,
		Senders:                 filter.Senders,
		Types:                   filter.Types,
		LazyLoadMembers:         filter.LazyLoadMembers,
		IncludeRedundantMembers: filter.IncludeRedundantMembers,
		NotRooms:                filter.NotRooms,
		Rooms:                   filter.Rooms,
		ContainsURL:             filter.ContainsURL,
	}

	id, requestedEvent, err := snapshot.SelectContextEvent(ctx, roomID, eventID)
	if err != nil {
		if err == sql.ErrNoRows {
			return util.JSONResponse{
				Code: http.StatusNotFound,
				JSON: jsonerror.NotFound(fmt.Sprintf("Event %s not found", eventID)),
			}
		}
		logrus.WithError(err).WithField("eventID", eventID).Error("unable to find requested event")
		return jsonerror.InternalServerError()
	}

	// verify the user is allowed to see the context for this room/event
	startTime := time.Now()
	filteredEvents, err := internal.ApplyHistoryVisibilityFilter(ctx, snapshot, rsAPI, []*gomatrixserverlib.HeaderedEvent{&requestedEvent}, nil, device.UserID, "context")
	if err != nil {
		logrus.WithError(err).Error("unable to apply history visibility filter")
		return jsonerror.InternalServerError()
	}
	logrus.WithFields(logrus.Fields{
		"duration": time.Since(startTime),
		"room_id":  roomID,
	}).Debug("applied history visibility (context)")
	if len(filteredEvents) == 0 {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: jsonerror.Forbidden("User is not allowed to query context"),
		}
	}

	eventsBefore, err := snapshot.SelectContextBeforeEvent(ctx, id, roomID, filter)
	if err != nil && err != sql.ErrNoRows {
		logrus.WithError(err).Error("unable to fetch before events")
		return jsonerror.InternalServerError()
	}

	_, eventsAfter, err := snapshot.SelectContextAfterEvent(ctx, id, roomID, filter)
	if err != nil && err != sql.ErrNoRows {
		logrus.WithError(err).Error("unable to fetch after events")
		return jsonerror.InternalServerError()
	}

	startTime = time.Now()
	eventsBeforeFiltered, eventsAfterFiltered, err := applyHistoryVisibilityOnContextEvents(ctx, snapshot, rsAPI, eventsBefore, eventsAfter, device.UserID)
	if err != nil {
		logrus.WithError(err).Error("unable to apply history visibility filter")
		return jsonerror.InternalServerError()
	}

	logrus.WithFields(logrus.Fields{
		"duration": time.Since(startTime),
		"room_id":  roomID,
	}).Debug("applied history visibility (context eventsBefore/eventsAfter)")

	// TODO: Get the actual state at the last event returned by SelectContextAfterEvent
	state, err := snapshot.CurrentState(ctx, roomID, &stateFilter, nil)
	if err != nil {
		logrus.WithError(err).Error("unable to fetch current room state")
		return jsonerror.InternalServerError()
	}

	eventsBeforeClient := gomatrixserverlib.HeaderedToClientEvents(eventsBeforeFiltered, gomatrixserverlib.FormatAll)
	eventsAfterClient := gomatrixserverlib.HeaderedToClientEvents(eventsAfterFiltered, gomatrixserverlib.FormatAll)

	newState := state
	if filter.LazyLoadMembers {
		allEvents := append(eventsBeforeFiltered, eventsAfterFiltered...)
		allEvents = append(allEvents, &requestedEvent)
		evs := gomatrixserverlib.HeaderedToClientEvents(allEvents, gomatrixserverlib.FormatAll)
		newState, err = applyLazyLoadMembers(ctx, device, snapshot, roomID, evs, lazyLoadCache)
		if err != nil {
			logrus.WithError(err).Error("unable to load membership events")
			return jsonerror.InternalServerError()
		}
	}

	ev := gomatrixserverlib.HeaderedToClientEvent(&requestedEvent, gomatrixserverlib.FormatAll)
	response := ContextRespsonse{
		Event:        &ev,
		EventsAfter:  eventsAfterClient,
		EventsBefore: eventsBeforeClient,
		State:        gomatrixserverlib.HeaderedToClientEvents(newState, gomatrixserverlib.FormatAll),
	}

	if len(response.State) > filter.Limit {
		response.State = response.State[len(response.State)-filter.Limit:]
	}
	start, end, err := getStartEnd(ctx, snapshot, eventsBefore, eventsAfter)
	if err == nil {
		response.End = end.String()
		response.Start = start.String()
	}
	succeeded = true
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: response,
	}
}

// applyHistoryVisibilityOnContextEvents is a helper function to avoid roundtrips to the roomserver
// by combining the events before and after the context event. Returns the filtered events,
// and an error, if any.
func applyHistoryVisibilityOnContextEvents(
	ctx context.Context, snapshot storage.DatabaseTransaction, rsAPI roomserver.SyncRoomserverAPI,
	eventsBefore, eventsAfter []*gomatrixserverlib.HeaderedEvent,
	userID string,
) (filteredBefore, filteredAfter []*gomatrixserverlib.HeaderedEvent, err error) {
	eventIDsBefore := make(map[string]struct{}, len(eventsBefore))
	eventIDsAfter := make(map[string]struct{}, len(eventsAfter))

	// Remember before/after eventIDs, so we can restore them
	// after applying history visibility checks
	for _, ev := range eventsBefore {
		eventIDsBefore[ev.EventID()] = struct{}{}
	}
	for _, ev := range eventsAfter {
		eventIDsAfter[ev.EventID()] = struct{}{}
	}

	allEvents := append(eventsBefore, eventsAfter...)
	filteredEvents, err := internal.ApplyHistoryVisibilityFilter(ctx, snapshot, rsAPI, allEvents, nil, userID, "context")
	if err != nil {
		return nil, nil, err
	}

	// "Restore" events in the correct context
	for _, ev := range filteredEvents {
		if _, ok := eventIDsBefore[ev.EventID()]; ok {
			filteredBefore = append(filteredBefore, ev)
		}
		if _, ok := eventIDsAfter[ev.EventID()]; ok {
			filteredAfter = append(filteredAfter, ev)
		}
	}
	return filteredBefore, filteredAfter, nil
}

func getStartEnd(ctx context.Context, snapshot storage.DatabaseTransaction, startEvents, endEvents []*gomatrixserverlib.HeaderedEvent) (start, end types.TopologyToken, err error) {
	if len(startEvents) > 0 {
		start, err = snapshot.EventPositionInTopology(ctx, startEvents[0].EventID())
		if err != nil {
			return
		}
	}
	if len(endEvents) > 0 {
		end, err = snapshot.EventPositionInTopology(ctx, endEvents[0].EventID())
	}
	return
}

func applyLazyLoadMembers(
	ctx context.Context,
	device *userapi.Device,
	snapshot storage.DatabaseTransaction,
	roomID string,
	events []gomatrixserverlib.ClientEvent,
	lazyLoadCache caching.LazyLoadCache,
) ([]*gomatrixserverlib.HeaderedEvent, error) {
	eventSenders := make(map[string]struct{})
	// get members who actually send an event
	for _, e := range events {
		// Don't add membership events the client should already know about
		if _, cached := lazyLoadCache.IsLazyLoadedUserCached(device, e.RoomID, e.Sender); cached {
			continue
		}
		eventSenders[e.Sender] = struct{}{}
	}

	wantUsers := make([]string, 0, len(eventSenders))
	for userID := range eventSenders {
		wantUsers = append(wantUsers, userID)
	}

	// Query missing membership events
	filter := gomatrixserverlib.DefaultStateFilter()
	filter.Senders = &wantUsers
	filter.Types = &[]string{gomatrixserverlib.MRoomMember}
	memberships, err := snapshot.GetStateEventsForRoom(ctx, roomID, &filter)
	if err != nil {
		return nil, err
	}

	// cache the membership events
	for _, membership := range memberships {
		lazyLoadCache.StoreLazyLoadedUser(device, roomID, *membership.StateKey(), membership.EventID())
	}

	return memberships, nil
}

func parseRoomEventFilter(req *http.Request) (*gomatrixserverlib.RoomEventFilter, error) {
	// Default room filter
	filter := &gomatrixserverlib.RoomEventFilter{Limit: 10}

	l := req.URL.Query().Get("limit")
	f := req.URL.Query().Get("filter")
	if l != "" {
		limit, err := strconv.Atoi(l)
		if err != nil {
			return nil, err
		}
		// NOTSPEC: feels like a good idea to have an upper bound limit
		if limit > 100 {
			limit = 100
		}
		filter.Limit = limit
	}
	if f != "" {
		if err := json.Unmarshal([]byte(f), &filter); err != nil {
			return nil, err
		}
	}

	return filter, nil
}
