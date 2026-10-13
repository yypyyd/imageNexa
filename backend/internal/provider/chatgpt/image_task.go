package chatgpt

import (
	"context"
	"errors"
	"strings"
)

var (
	ErrTaskAccepted          = errors.New("chatgpt image task accepted; recovery pending")
	ErrTaskSubmissionUnknown = errors.New("chatgpt image submission outcome unknown; do not resubmit")
)

// ImageTaskError deliberately hides transport/auth errors after submission from
// account failover. A failed download does not authorize another generation.
type ImageTaskError struct {
	ConversationID string
	Cause          error
}

func (e *ImageTaskError) Error() string {
	if e.ConversationID == "" {
		return ErrTaskSubmissionUnknown.Error()
	}
	return ErrTaskAccepted.Error()
}

func (e *ImageTaskError) Is(target error) bool {
	if target == ErrContentPolicy {
		return errors.Is(e.Cause, ErrContentPolicy)
	}
	return (target == ErrTaskAccepted && e.ConversationID != "") ||
		(target == ErrTaskSubmissionUnknown && e.ConversationID == "")
}

func AcceptedFailure(conversationID string, cause error) error {
	return &ImageTaskError{ConversationID: strings.TrimSpace(conversationID), Cause: cause}
}

func AcceptedTaskID(err error) string {
	var task *ImageTaskError
	if errors.As(err, &task) {
		return task.ConversationID
	}
	return ""
}

// ResumeImageTask only reads the existing conversation; it never submits work.
func (c *Client) ResumeImageTask(ctx context.Context, accessToken, conversationID string) ([]byte, map[string]any, error) {
	session, err := c.newProxySession(accessToken)
	if err != nil {
		return nil, nil, AcceptedFailure(conversationID, err)
	}
	conversation, err := c.getConversation(ctx, session, accessToken, conversationID)
	if err != nil {
		return nil, nil, AcceptedFailure(conversationID, err)
	}
	refs := conversationReferenceIDs(conversation)
	files, sediments := extractImageIDs(conversation)
	files, sediments, err = c.pollForImage(ctx, session, accessToken, conversationID, files, sediments, refs, pollBudget(ctx))
	if err != nil {
		return nil, nil, AcceptedFailure(conversationID, err)
	}
	urls, err := c.resolveImageURLs(ctx, session, accessToken, conversationID, files, sediments)
	if err != nil {
		return nil, nil, AcceptedFailure(conversationID, err)
	}
	if len(urls) == 0 {
		return nil, nil, AcceptedFailure(conversationID, ErrTemporaryUpstream)
	}
	return nil, map[string]any{"conversation_id": conversationID, "image_url": urls[0]}, nil
}

// References may be echoed by assistant/tool messages. Recover their IDs from
// user messages before inspecting generated assets, including after a restart.
func conversationReferenceIDs(conversation map[string]any) map[string]bool {
	refs := map[string]bool{}
	mapping, _ := conversation["mapping"].(map[string]any)
	for _, raw := range mapping {
		node, _ := raw.(map[string]any)
		message, _ := node["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		if strings.EqualFold(stringValue(author["role"]), "user") {
			var files, sediments []string
			walkForIDs(message["content"], &files, &sediments)
			walkForIDs(message["metadata"], &files, &sediments)
			for _, id := range append(files, sediments...) {
				refs[id] = true
			}
		}
	}
	return refs
}
