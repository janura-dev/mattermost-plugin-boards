// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package notifysubscriptions

import (
	"errors"
	"testing"
	"time"

	"github.com/mattermost/mattermost-plugin-boards/server/model"
	"github.com/mattermost/mattermost-plugin-boards/server/services/notify"
	"github.com/mattermost/mattermost-plugin-boards/server/services/permissions"
	"github.com/stretchr/testify/require"

	mm_model "github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

func assignmentBoard() *model.Board {
	return &model.Board{ID: "board", TeamID: "team", CardProperties: []map[string]interface{}{
		{"id": "owner", "name": "Responsabile", "type": "person"},
		{"id": "reviewers", "name": "Reviewers", "type": "multiPerson"},
		{"id": "status", "name": "Status", "type": "select"},
	}}
}

func assignmentCard(properties map[string]interface{}) *model.Block {
	return &model.Block{ID: "card", BoardID: "board", Type: model.TypeCard, Title: "Saluto",
		Fields: map[string]interface{}{model.BlockFieldProperties: properties}}
}

func TestNewAssignees(t *testing.T) {
	tests := []struct {
		name         string
		old, current map[string]interface{}
		want         []string
	}{
		{"creation", nil, map[string]interface{}{"owner": "alice"}, []string{"alice"}},
		{"reassignment", map[string]interface{}{"owner": "alice"}, map[string]interface{}{"owner": "bob"}, []string{"bob"}},
		{"unrelated update", map[string]interface{}{"owner": "alice"}, map[string]interface{}{"owner": "alice", "status": "done"}, nil},
		{"unassignment", map[string]interface{}{"owner": "alice"}, map[string]interface{}{"owner": ""}, nil},
		{"multiple people from JSON", map[string]interface{}{"reviewers": []interface{}{"alice"}},
			map[string]interface{}{"reviewers": []interface{}{"alice", "bob", "bob", ""}}, []string{"bob"}},
		{"multiple people in memory", nil, map[string]interface{}{"reviewers": []string{"alice", "bob"}}, []string{"alice", "bob"}},
		{"same person in two fields", nil, map[string]interface{}{"owner": "alice", "reviewers": []string{"alice"}}, []string{"alice"}},
		{"move between person fields", map[string]interface{}{"owner": "alice"}, map[string]interface{}{"reviewers": []string{"alice"}}, nil},
		{"non-person properties", nil, map[string]interface{}{"status": "alice", "unknown": "bob"}, nil},
		{"malformed values", nil, map[string]interface{}{"owner": []string{"alice"}, "reviewers": []interface{}{1, nil, "bob"}}, []string{"bob"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var old *model.Block
			if tt.old != nil {
				old = assignmentCard(tt.old)
			}
			require.Equal(t, tt.want, newAssignees(assignmentBoard(), assignmentCard(tt.current), old))
		})
	}
}

type assignmentApp struct {
	AppAPI
	subs        []*model.Subscriber
	created     []string
	createErr   error
	lookupErr   error
	subsErr     error
	deleted     bool
	newFollowAt int64
}

func (a *assignmentApp) GetSubscribersForBlock(string) ([]*model.Subscriber, error) {
	return a.subs, a.subsErr
}
func (a *assignmentApp) GetUserByID(id string) (*model.User, error) {
	user := &model.User{ID: id, Username: id}
	if a.deleted {
		user.DeleteAt = 1
	}
	return user, a.lookupErr
}
func (a *assignmentApp) CreateSubscription(sub *model.Subscription) (*model.Subscription, error) {
	a.created = append(a.created, sub.SubscriberID)
	if a.createErr == nil {
		a.subs = append(a.subs, &model.Subscriber{SubscriberType: sub.SubscriberType, SubscriberID: sub.SubscriberID, NotifiedAt: a.newFollowAt})
	}
	return sub, a.createErr
}
func (a *assignmentApp) UpsertNotificationHint(hint *model.NotificationHint, _ time.Duration) (*model.NotificationHint, error) {
	return hint, nil
}

type assignmentPermissions struct {
	permissions.PermissionsService
	denied bool
}

func (p assignmentPermissions) HasPermissionToBoard(_, _ string, _ *mm_model.Permission) bool {
	return !p.denied
}

type assignmentDelivery struct {
	users       []string
	attachments []*mm_model.SlackAttachment
	err         error
}

func (d *assignmentDelivery) SubscriptionDeliverSlackAttachments(_, userID string, _ model.SubscriberType,
	attachments []*mm_model.SlackAttachment) error {
	d.users = append(d.users, userID)
	d.attachments = append(d.attachments, attachments...)
	return d.err
}

func assignmentBackend(t *testing.T, app *assignmentApp, delivery *assignmentDelivery, denied bool) *Backend {
	t.Helper()
	return New(BackendParams{ServerRoot: "https://chat.example.com", AppAPI: app, Delivery: delivery,
		Permissions: assignmentPermissions{denied: denied}, Logger: mlog.CreateConsoleTestLogger(t)})
}

func assignmentEvent() notify.BlockChangeEvent {
	card := assignmentCard(map[string]interface{}{"owner": "alice"})
	return notify.BlockChangeEvent{Action: notify.Update, Board: assignmentBoard(), Card: card, BlockChanged: card,
		BlockOld: assignmentCard(nil), ModifiedBy: &model.BoardMember{UserID: "author"}}
}

func TestAssignmentNotificationAndFollow(t *testing.T) {
	for _, action := range []notify.Action{notify.Add, notify.Update} {
		t.Run(string(action), func(t *testing.T) {
			app, delivery := &assignmentApp{}, &assignmentDelivery{}
			backend := assignmentBackend(t, app, delivery, false)
			evt := assignmentEvent()
			evt.Action = action
			if action == notify.Add {
				evt.BlockOld = nil
			}
			require.NoError(t, backend.BlockChanged(evt))
			require.Contains(t, app.created, "alice")
			require.Equal(t, []string{"alice"}, delivery.users)
			require.Equal(t, "Saluto", delivery.attachments[0].Title)
			require.Contains(t, delivery.attachments[0].TitleLink, "https://chat.example.com/")
			require.Contains(t, delivery.attachments[0].TitleLink, "card")

			// Saving unrelated fields does not re-subscribe or notify again.
			evt.Action, evt.BlockOld = notify.Update, evt.BlockChanged
			evt.BlockChanged = assignmentCard(map[string]interface{}{"owner": "alice", "status": "done"})
			require.NoError(t, backend.BlockChanged(evt))
			require.Equal(t, []string{"alice"}, delivery.users)
		})
	}
}

func TestAssignmentExistingFollower(t *testing.T) {
	app := &assignmentApp{subs: []*model.Subscriber{{SubscriberID: "alice", SubscriberType: model.SubTypeUser, NotifiedAt: 123}}}
	delivery := &assignmentDelivery{}
	require.NoError(t, assignmentBackend(t, app, delivery, false).notifyAssignments(assignmentEvent()))
	require.Empty(t, app.created, "existing follow cursor must not be reset")
	require.Equal(t, int64(123), app.subs[0].NotifiedAt)
	require.Equal(t, []string{"alice"}, delivery.users)
}

func TestReassignmentKeepsFormerFollower(t *testing.T) {
	app := &assignmentApp{subs: []*model.Subscriber{{SubscriberID: "alice", SubscriberType: model.SubTypeUser}}}
	delivery := &assignmentDelivery{}
	backend := assignmentBackend(t, app, delivery, false)
	evt := assignmentEvent()
	evt.BlockOld = evt.BlockChanged
	evt.BlockChanged = assignmentCard(map[string]interface{}{"owner": "bob", "reviewers": []interface{}{"bob", "charlie"}})
	require.NoError(t, backend.notifyAssignments(evt))
	require.Equal(t, []string{"bob", "charlie"}, delivery.users)
	require.Equal(t, []string{"bob", "charlie"}, app.created)
	require.Len(t, app.subs, 3, "reassignment must not remove the former follower")
}

func TestAssignmentIgnoredEvents(t *testing.T) {
	tests := []struct {
		name            string
		change          func(*notify.BlockChangeEvent)
		denied, deleted bool
		followed        bool
	}{
		{name: "self assignment", change: func(e *notify.BlockChangeEvent) { e.ModifiedBy.UserID = "alice" }, followed: true},
		{name: "no access", denied: true},
		{name: "deactivated assignee", deleted: true},
		{name: "card template", change: func(e *notify.BlockChangeEvent) { e.BlockChanged.Fields["isTemplate"] = true }},
		{name: "board template", change: func(e *notify.BlockChangeEvent) { e.Board.IsTemplate = true }},
		{name: "deleted card", change: func(e *notify.BlockChangeEvent) { e.BlockChanged.DeleteAt = 1 }},
		{name: "delete action", change: func(e *notify.BlockChangeEvent) { e.Action = notify.Delete }},
		{name: "comment", change: func(e *notify.BlockChangeEvent) { e.BlockChanged.Type = model.TypeComment }},
		{name: "missing board", change: func(e *notify.BlockChangeEvent) { e.Board = nil }},
		{name: "missing card", change: func(e *notify.BlockChangeEvent) { e.Card = nil }},
		{name: "missing block", change: func(e *notify.BlockChangeEvent) { e.BlockChanged = nil }},
		{name: "missing author", change: func(e *notify.BlockChangeEvent) { e.ModifiedBy = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := assignmentEvent()
			if tt.change != nil {
				tt.change(&evt)
			}
			app, delivery := &assignmentApp{deleted: tt.deleted}, &assignmentDelivery{}
			require.NoError(t, assignmentBackend(t, app, delivery, tt.denied).notifyAssignments(evt))
			require.Empty(t, delivery.users)
			if tt.followed {
				require.Equal(t, []string{"alice"}, app.created)
			} else {
				require.Empty(t, app.created)
			}
		})
	}
}

func TestAssignmentFailures(t *testing.T) {
	failure := errors.New("service unavailable")
	for _, kind := range []string{"lookup", "subscription", "delivery", "subscribers"} {
		t.Run(kind, func(t *testing.T) {
			app, delivery := &assignmentApp{}, &assignmentDelivery{}
			switch kind {
			case "lookup":
				app.lookupErr = failure
			case "subscription":
				app.createErr = failure
			case "delivery":
				delivery.err = failure
			case "subscribers":
				app.subsErr = failure
			}
			err := assignmentBackend(t, app, delivery, false).notifyAssignments(assignmentEvent())
			require.ErrorContains(t, err, failure.Error())
			if kind == "subscription" {
				require.Equal(t, []string{"alice"}, delivery.users, "follow failure must not prevent the assignment alert")
			}
		})
	}
}

func TestAssignmentSummaryDoesNotDuplicateAlert(t *testing.T) {
	board := assignmentBoard()
	diff := &Diff{Board: board, Card: assignmentCard(nil), BlockType: model.TypeCard,
		OldBlock: assignmentCard(nil), NewBlock: assignmentCard(map[string]interface{}{"owner": "alice"}),
		Authors: StringMap{"author": "Author"}, PropDiffs: []PropDiff{{ID: "owner", Name: "Responsabile", NewValue: "alice"}}}
	opts := DiffConvOpts{Logger: mlog.CreateConsoleTestLogger(t)}
	for _, userID := range []string{"alice", "other-follower"} {
		attachments, err := Diffs2SlackAttachments(subscriptionDiffs([]*Diff{diff}, userID), opts)
		require.NoError(t, err)
		if userID == "alice" {
			require.Empty(t, attachments)
		} else {
			require.Len(t, attachments, 1)
		}
	}
	require.Len(t, diff.PropDiffs, 1, "filtering must not mutate other followers' summaries")
	diff.PropDiffs = append(diff.PropDiffs, PropDiff{ID: "status", Name: "Status", NewValue: "done"})
	attachments, err := Diffs2SlackAttachments(subscriptionDiffs([]*Diff{diff}, "alice"), opts)
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Len(t, attachments[0].Fields, 1)
	require.Equal(t, "Status", attachments[0].Fields[0].Title)

	diff.OldBlock = nil
	require.Empty(t, subscriptionDiffs([]*Diff{diff}, "alice"), "creation alert must not be sent twice")
	require.Len(t, subscriptionDiffs([]*Diff{diff}, "other-follower"), 1)
	diff.Diffs = []*Diff{{BlockType: model.TypeComment, Authors: StringMap{"author": "Author"},
		NewBlock: &model.Block{Type: model.TypeComment, Title: "An update after assignment"}}}
	attachments, err = Diffs2SlackAttachments(subscriptionDiffs([]*Diff{diff}, "alice"), opts)
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Len(t, attachments[0].Fields, 1)
	require.Contains(t, attachments[0].Fields[0].Value, "An update after assignment")
}

type assignmentHistoryApp struct {
	*assignmentApp
	board   *model.Board
	history []*model.Block // newest first, like the store
}

func (a *assignmentHistoryApp) GetBoardAndCardByID(string) (*model.Board, *model.Block, error) {
	return a.board, a.history[0], nil
}
func (a *assignmentHistoryApp) GetBlockHistory(_ string, opts model.QueryBlockHistoryOptions) ([]*model.Block, error) {
	var blocks []*model.Block
	for _, block := range a.history {
		if opts.BeforeUpdateAt != 0 && block.UpdateAt >= opts.BeforeUpdateAt {
			continue
		}
		if opts.AfterUpdateAt != 0 && block.UpdateAt <= opts.AfterUpdateAt {
			continue
		}
		blocks = append(blocks, block)
		if opts.Limit != 0 && uint64(len(blocks)) >= opts.Limit {
			break
		}
	}
	return blocks, nil
}
func (a *assignmentHistoryApp) GetBlockHistoryNewestChildren(string, model.QueryBlockHistoryChildOptions) ([]*model.Block, bool, error) {
	return nil, false, nil
}
func (a *assignmentHistoryApp) UpdateSubscribersNotifiedAt(_ string, notifiedAt int64) error {
	for _, sub := range a.subs {
		sub.NotifiedAt = notifiedAt
	}
	return nil
}

func TestAssignmentAndSubsequentSummary(t *testing.T) {
	board := assignmentBoard()
	board.CardProperties = append(board.CardProperties, map[string]interface{}{"id": "note", "name": "Note", "type": "text"})
	before := assignmentCard(map[string]interface{}{"note": "old note"})
	assigned := assignmentCard(map[string]interface{}{"owner": "alice", "note": "old note"})
	later := assignmentCard(map[string]interface{}{"owner": "alice", "note": "new note"})
	before.UpdateAt, assigned.UpdateAt, later.UpdateAt = 100, 200, 300
	for _, block := range []*model.Block{before, assigned, later} {
		block.ModifiedBy = "author"
	}
	for _, subsequentUpdate := range []bool{false, true} {
		t.Run(map[bool]string{false: "assignment only", true: "later update retained"}[subsequentUpdate], func(t *testing.T) {
			app := &assignmentHistoryApp{assignmentApp: &assignmentApp{newFollowAt: 250,
				subs: []*model.Subscriber{{SubscriberID: "other-follower", SubscriberType: model.SubTypeUser, NotifiedAt: 100}}},
				board: board, history: []*model.Block{assigned, before}}
			delivery := &assignmentDelivery{}
			backend := New(BackendParams{ServerRoot: "https://chat.example.com", AppAPI: app, Delivery: delivery,
				Permissions: assignmentPermissions{}, Logger: mlog.CreateConsoleTestLogger(t)})
			evt := notify.BlockChangeEvent{Action: notify.Update, Board: board, Card: assigned, BlockChanged: assigned,
				BlockOld: before, ModifiedBy: &model.BoardMember{UserID: "author"}}
			require.NoError(t, backend.BlockChanged(evt))
			require.Equal(t, []string{"alice"}, delivery.users)
			if subsequentUpdate {
				app.history = []*model.Block{later, assigned, before}
			}
			require.NoError(t, backend.notifier.notifySubscribers(&model.NotificationHint{BlockID: "card", BlockType: model.TypeCard}))
			if subsequentUpdate {
				require.Equal(t, []string{"alice", "other-follower", "alice"}, delivery.users)
				require.Len(t, delivery.attachments[2].Fields, 1)
				require.Equal(t, "Note", delivery.attachments[2].Fields[0].Title)
			} else {
				require.Equal(t, []string{"alice", "other-follower"}, delivery.users)
			}
		})
	}
}
