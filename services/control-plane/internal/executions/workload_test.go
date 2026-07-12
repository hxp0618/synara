package executions

import (
	"strings"
	"testing"
)

func TestBoundConversationHistoryKeepsNewestCompleteMessages(t *testing.T) {
	messages := []ConversationMessage{
		{Role: "user", Text: "old question"},
		{Role: "assistant", Text: "old answer"},
		{Role: "user", Text: "new question"},
		{Role: "assistant", Text: "new answer"},
	}
	bounded := boundConversationHistory(messages, len("usernew question")+len("assistantnew answer"))
	if len(bounded) != 2 || bounded[0].Text != "new question" || bounded[1].Text != "new answer" {
		t.Fatalf("history bound did not keep the newest complete messages: %#v", bounded)
	}
}

func TestBoundConversationHistoryTruncatesSingleOversizedNewestMessage(t *testing.T) {
	bounded := boundConversationHistory([]ConversationMessage{{Role: "assistant", Text: strings.Repeat("x", 32)}}, 8)
	if len(bounded) != 1 || bounded[0].Text != strings.Repeat("x", 8) {
		t.Fatalf("oversized newest message was not bounded: %#v", bounded)
	}
}
