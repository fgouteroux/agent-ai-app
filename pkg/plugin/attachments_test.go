package plugin

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestAttachmentMaxBytes_DefaultsWhenZero(t *testing.T) {
	if got := attachmentMaxBytes(Settings{}); got != defaultAttachmentMaxBytes {
		t.Errorf("expected default %d, got %d", defaultAttachmentMaxBytes, got)
	}
	if got := attachmentMaxBytes(Settings{AttachmentMaxBytes: 1234}); got != 1234 {
		t.Errorf("expected explicit value 1234, got %d", got)
	}
}

func TestValidateAttachments_RejectsOversized(t *testing.T) {
	big := strings.Repeat("a", 100)
	err := validateAttachments([]ChatAttachment{{Name: "big.txt", Content: big, Type: "text"}}, 50)
	if err == nil {
		t.Fatal("expected an error for an oversized attachment")
	}
}

func TestValidateAttachments_AllowsWithinLimit(t *testing.T) {
	err := validateAttachments([]ChatAttachment{{Name: "small.txt", Content: "hello", Type: "text"}}, 50)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateAttachments_RejectsTooManyFiles(t *testing.T) {
	attachments := make([]ChatAttachment, maxAttachmentsPerMessage+1)
	for i := range attachments {
		attachments[i] = ChatAttachment{Name: "f.txt", Content: "hi", Type: "text"}
	}
	err := validateAttachments(attachments, 50)
	if err == nil {
		t.Fatal("expected an error for exceeding the per-message attachment count")
	}
}

func TestValidateAttachments_AllowsExactlyMaxCount(t *testing.T) {
	attachments := make([]ChatAttachment, maxAttachmentsPerMessage)
	for i := range attachments {
		attachments[i] = ChatAttachment{Name: "f.txt", Content: "hi", Type: "text"}
	}
	if err := validateAttachments(attachments, 50); err != nil {
		t.Fatalf("expected no error at exactly the max count, got: %v", err)
	}
}

func TestBuildUserMessage_TextOnly(t *testing.T) {
	msg := buildUserMessage("what's in this file?", []ChatAttachment{
		{Name: "config.yaml", Content: "replicas: 3", Type: "text"},
	})
	if msg.MultiContent != nil {
		t.Fatalf("expected plain Content for text-only attachments, got MultiContent: %+v", msg.MultiContent)
	}
	if !strings.Contains(msg.Content, "what's in this file?") {
		t.Error("expected prompt text in message content")
	}
	if !strings.Contains(msg.Content, "config.yaml") || !strings.Contains(msg.Content, "replicas: 3") {
		t.Errorf("expected attachment name and content framed in message, got: %q", msg.Content)
	}
}

func TestBuildUserMessage_WithImage(t *testing.T) {
	msg := buildUserMessage("what does this show?", []ChatAttachment{
		{Name: "screenshot.png", Content: "ZmFrZWJhc2U2NA==", Type: "image", MimeType: "image/png"},
	})
	if msg.Content != "" {
		t.Errorf("expected empty Content when an image is present, got %q", msg.Content)
	}
	if len(msg.MultiContent) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d: %+v", len(msg.MultiContent), msg.MultiContent)
	}
	if msg.MultiContent[0].Type != openai.ChatMessagePartTypeText {
		t.Errorf("expected first part to be text, got %s", msg.MultiContent[0].Type)
	}
	if msg.MultiContent[1].Type != openai.ChatMessagePartTypeImageURL {
		t.Errorf("expected second part to be image_url, got %s", msg.MultiContent[1].Type)
	}
	if msg.MultiContent[1].ImageURL == nil || !strings.HasPrefix(msg.MultiContent[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("expected a data: URL with the given mime type, got: %+v", msg.MultiContent[1].ImageURL)
	}
}

func TestBuildUserMessage_NoAttachments(t *testing.T) {
	msg := buildUserMessage("just a question", nil)
	if msg.Content != "just a question" {
		t.Errorf("expected content to be exactly the prompt, got %q", msg.Content)
	}
	if msg.MultiContent != nil {
		t.Error("expected no MultiContent with no attachments")
	}
}
