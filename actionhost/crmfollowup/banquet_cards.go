package crmfollowup

import (
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Field labels are fixed business schema, never model-provided Markdown.
var banquetLabels = map[string]core.MsgKey{
	"version":          core.MsgBanquetVersion,
	"recipient":        core.MsgBanquetRecipient,
	"status":           core.MsgBanquetStatus,
	"missing":          core.MsgBanquetMissing,
	"return_reason":    core.MsgBanquetReturnReason,
	"materials":        core.MsgBanquetMaterials,
	"name":             core.MsgBanquetName,
	"number":           core.MsgBanquetNumber,
	"source":           core.MsgBanquetSource,
	"owner":            core.MsgCRMFieldOwner,
	"banquet_date":     core.MsgBanquetBanquetDate,
	"date_note":        core.MsgBanquetDateNote,
	"date_status":      core.MsgBanquetDateStatus,
	"session":          core.MsgBanquetSession,
	"venue":            core.MsgBanquetVenue,
	"estimated_tables": core.MsgBanquetEstimatedTables,
	"budget":           core.MsgBanquetBudget,
	"visit_at":         core.MsgBanquetVisitAt,
	"visited_at":       core.MsgBanquetVisitedAt,
	"stage":            core.MsgCRMFieldStage,
	"next_action":      core.MsgCRMFieldNextAction,
	"next_followup_at": core.MsgCRMFieldNextFollowupAt,
	"followup_status":  core.MsgBanquetFollowupStatus,
	"occurred_at":      core.MsgCRMFieldOccurredAt,
	"content":          core.MsgCRMFieldContent,
	"channel":          core.MsgCRMFieldChannel,
}

func renderBanquet(preview map[string]any, i18n *core.I18n) string {
	var sections []string
	if actor := text(preview["actor"]); actor != "" {
		sections = append(sections, i18n.Tf(core.MsgCRMPreviewActorFmt, display(actor)))
	}
	if customer, ok := preview["customer"].(map[string]any); ok {
		sections = append(sections, renderFields(customer, []string{"customer_number", "name", "contact"}, i18n))
	}
	if target, ok := preview["banquet"].(map[string]any); ok && target["name"] != nil {
		sections = append(sections, display(target["name"]))
	}
	if handoff, ok := preview["handoff"].(map[string]any); ok {
		sections = append(sections, renderHandoff(handoff, i18n))
	}
	changes, _ := preview["banquet_changes"].([]any)
	for _, raw := range changes {
		change, _ := raw.(map[string]any)
		key := text(change["field"])
		label, ok := banquetLabels[key]
		if !ok {
			continue
		}
		sections = append(sections, fmt.Sprintf("- **%s**: %s → %s", i18n.T(label),
			displayField(key, change["before"], i18n), displayField(key, change["after"], i18n)))
	}
	sections = append(sections, i18n.Tf(core.MsgCRMProfileExpiryBlockFmt, displayField("expires_at", preview["expires_at"], i18n)))
	return strings.Join(sections, "\n\n")
}

func renderHandoff(row map[string]any, i18n *core.I18n) string {
	var lines []string
	for _, key := range []string{"name", "version", "recipient", "status", "missing", "return_reason", "materials"} {
		if value, ok := row[key]; ok {
			if key == "materials" {
				materials, _ := value.([]any)
				for _, raw := range materials {
					material, _ := raw.(map[string]any)
					lines = append(lines, fmt.Sprintf("- %s · v%s · SHA256 %s", display(material["name"]), display(material["version"]), display(material["sha256"])))
				}
				continue
			}
			field := key
			if key == "recipient" {
				field = "owner"
			}
			lines = append(lines, fmt.Sprintf("- **%s**: %s", i18n.T(banquetLabels[key]), displayField(field, value, i18n)))
		}
	}
	if link := safeLink(row["url"]); link != "" {
		lines = append(lines, "["+i18n.T(core.MsgBanquetOpenRecord)+"]("+link+")")
	}
	return strings.Join(lines, "\n")
}

func renderBanquetReceipt(row map[string]any, i18n *core.I18n) string {
	var lines []string
	for _, key := range []string{"number", "name", "owner", "stage", "banquet_date", "date_status", "date_note", "session", "venue", "estimated_tables", "budget", "visit_at", "visited_at", "next_action", "next_followup_at", "followup_status"} {
		if value, ok := row[key]; ok {
			lines = append(lines, fmt.Sprintf("- **%s**: %s", i18n.T(banquetLabels[key]), displayField(key, value, i18n)))
		}
	}
	if link := safeLink(row["url"]); link != "" {
		lines = append(lines, "["+i18n.T(core.MsgBanquetOpenRecord)+"]("+link+")")
	}
	return strings.Join(lines, "\n")
}
