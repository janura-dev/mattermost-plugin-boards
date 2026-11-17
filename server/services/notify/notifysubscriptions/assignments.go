// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package notifysubscriptions

import (
	"fmt"
	"slices"

	"github.com/mattermost/mattermost-plugin-boards/server/model"
	"github.com/mattermost/mattermost-plugin-boards/server/services/notify"
	"github.com/mattermost/mattermost-plugin-boards/server/utils"
	"github.com/wiggin77/merror"

	mm_model "github.com/mattermost/mattermost/server/public/model"
)

// assignedUsers uses property types rather than names so renamed/localized person
// properties and multi-person assignments behave the same way.
func assignedUsers(board *model.Board, card *model.Block) map[string]bool {
	users := make(map[string]bool)
	if board == nil || card == nil {
		return users
	}
	properties, _ := card.Fields[model.BlockFieldProperties].(map[string]interface{})
	for _, property := range board.CardProperties {
		id, _ := property["id"].(string)
		switch property["type"] {
		case "person":
			if userID, ok := properties[id].(string); ok && userID != "" {
				users[userID] = true
			}
		case "multiPerson":
			switch value := properties[id].(type) {
			case []string:
				for _, userID := range value {
					if userID != "" {
						users[userID] = true
					}
				}
			case []interface{}:
				for _, item := range value {
					if userID, ok := item.(string); ok && userID != "" {
						users[userID] = true
					}
				}
			}
		}
	}
	return users
}

func newAssignees(board *model.Board, card, oldCard *model.Block) []string {
	oldUsers := assignedUsers(board, oldCard)
	var users []string
	for userID := range assignedUsers(board, card) {
		if !oldUsers[userID] {
			users = append(users, userID)
		}
	}
	slices.Sort(users)
	return users
}

func isAssignmentCard(board *model.Board, card *model.Block) bool {
	if board == nil || board.IsTemplate || card == nil || card.Type != model.TypeCard || card.DeleteAt != 0 {
		return false
	}
	isTemplate, _ := card.Fields["isTemplate"].(bool)
	return !isTemplate
}

func (b *Backend) notifyAssignments(evt notify.BlockChangeEvent) error {
	if !isAssignmentCard(evt.Board, evt.BlockChanged) || evt.Card == nil || evt.ModifiedBy == nil ||
		(evt.Action != notify.Add && evt.Action != notify.Update) {
		return nil
	}
	users := newAssignees(evt.Board, evt.BlockChanged, evt.BlockOld)
	if len(users) == 0 {
		return nil
	}

	subs, err := b.appAPI.GetSubscribersForBlock(evt.Card.ID)
	if err != nil {
		return fmt.Errorf("cannot fetch subscribers for assigned card %s: %w", evt.Card.ID, err)
	}
	following := make(map[string]bool)
	for _, sub := range subs {
		if sub.SubscriberType == model.SubTypeUser {
			following[sub.SubscriberID] = true
		}
	}

	merr := merror.New()
	for _, userID := range users {
		// Assignment never grants access to a board.
		if !b.permissions.HasPermissionToBoard(userID, evt.Board.ID, model.PermissionViewBoard) {
			continue
		}
		user, err := b.appAPI.GetUserByID(userID)
		if err != nil {
			merr.Append(fmt.Errorf("cannot fetch assignee %s: %w", userID, err))
			continue
		}
		if user == nil || user.DeleteAt != 0 {
			continue
		}
		if !following[userID] {
			_, err = b.appAPI.CreateSubscription(&model.Subscription{
				BlockType: model.TypeCard, BlockID: evt.Card.ID,
				SubscriberType: model.SubTypeUser, SubscriberID: userID,
			})
			if err != nil {
				merr.Append(fmt.Errorf("cannot subscribe assignee %s to card %s: %w", userID, evt.Card.ID, err))
			}
		}
		if userID == evt.ModifiedBy.UserID {
			continue
		}
		link := utils.MakeCardLink(b.serverRoot, evt.Board.TeamID, evt.Board.ID, evt.Card.ID)
		attachments := []*mm_model.SlackAttachment{{
			Pretext: "You were assigned a card.",
			Title:   evt.BlockChanged.Title, TitleLink: link,
			Fallback: fmt.Sprintf("You were assigned a card: %s — %s", evt.BlockChanged.Title, link),
		}}
		if err := b.delivery.SubscriptionDeliverSlackAttachments(evt.Board.TeamID, userID, model.SubTypeUser, attachments); err != nil {
			merr.Append(fmt.Errorf("cannot notify assignee %s for card %s: %w", userID, evt.Card.ID, err))
		}
	}
	return merr.ErrorOrNil()
}

// subscriptionDiffs omits the assignment already delivered immediately to this
// assignee. Other followers still see it, and unrelated changes remain in the
// assignee's normal summary. Never mutate the shared diffs used by other users.
func subscriptionDiffs(diffs []*Diff, userID string) []*Diff {
	result := make([]*Diff, 0, len(diffs))
	for _, diff := range diffs {
		if !isAssignmentCard(diff.Board, diff.NewBlock) ||
			!slices.Contains(newAssignees(diff.Board, diff.NewBlock, diff.OldBlock), userID) {
			result = append(result, diff)
			continue
		}
		if diff.OldBlock == nil {
			// Keep comments/content added after creation, but omit the creation
			// itself, which has its own immediate assignment notification.
			if len(diff.Diffs) > 0 {
				copyDiff := *diff
				copyDiff.OldBlock = diff.NewBlock
				copyDiff.PropDiffs = nil
				result = append(result, &copyDiff)
			}
			continue
		}
		copyDiff := *diff
		copyDiff.PropDiffs = make([]PropDiff, 0, len(diff.PropDiffs))
		for _, propDiff := range diff.PropDiffs {
			propertyBoard := &model.Board{}
			for _, property := range diff.Board.CardProperties {
				if property["id"] == propDiff.ID {
					propertyBoard.CardProperties = append(propertyBoard.CardProperties, property)
				}
			}
			if !assignedUsers(propertyBoard, diff.NewBlock)[userID] {
				copyDiff.PropDiffs = append(copyDiff.PropDiffs, propDiff)
			}
		}
		result = append(result, &copyDiff)
	}
	return result
}
