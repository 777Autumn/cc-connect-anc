package crmfollowup

import (
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Field labels are fixed business schema, never model-provided Markdown.
var banquetLabels = map[string][5]string{
	"version":          {"Handoff version", "交接版本", "交接版本", "引継ぎ版", "Versión"},
	"recipient":        {"Named recipient", "指定接手人", "指定接手人", "引継ぎ先", "Destinatario"},
	"status":           {"Acknowledgment status", "确认状态", "確認狀態", "確認状況", "Confirmación"},
	"missing":          {"Missing information", "待补事项", "待補事項", "不足事項", "Información pendiente"},
	"return_reason":    {"Return reason", "退回原因", "退回原因", "差戻し理由", "Motivo de devolución"},
	"materials":        {"Exact material versions", "材料及确切版本", "材料及確切版本", "資料と版", "Materiales y versiones"},
	"name":             {"Banquet", "宴会名称", "宴會名稱", "宴会名", "Banquete"},
	"number":           {"Business number", "业务编号", "業務編號", "案件番号", "Número"},
	"source":           {"Source", "来源", "來源", "流入元", "Origen"},
	"owner":            {"Sales owner", "销售负责人", "銷售負責人", "営業担当", "Responsable"},
	"banquet_date":     {"Banquet date", "宴会日期", "宴會日期", "宴会日", "Fecha"},
	"date_note":        {"Unconfirmed date details", "日期待确认说明", "日期待確認說明", "日付の未確認事項", "Fecha por confirmar"},
	"date_status":      {"Date certainty", "日期确认状态", "日期確認狀態", "日付の確定状況", "Confirmación de fecha"},
	"session":          {"Time slot", "宴会时段", "宴會時段", "時間帯", "Horario"},
	"venue":            {"Venue", "场地", "場地", "会場", "Lugar"},
	"estimated_tables": {"Estimated tables", "预计桌数", "預計桌數", "予定卓数", "Mesas estimadas"},
	"budget":           {"Budget", "预算", "預算", "予算", "Presupuesto"},
	"visit_at":         {"Scheduled visit", "到店安排", "到店安排", "来店予定", "Visita prevista"},
	"visited_at":       {"Actual visit", "实际到店时间", "實際到店時間", "実際の来店日時", "Visita realizada"},
	"stage":            {"Sales stage", "销售阶段", "銷售階段", "営業段階", "Etapa comercial"},
	"next_action":      {"Next step", "下一步", "下一步", "次の行動", "Próximo paso"},
	"next_followup_at": {"Follow-up time", "跟进时间", "跟進時間", "フォロー日時", "Fecha de seguimiento"},
	"followup_status":  {"Follow-up status", "跟进状态", "跟進狀態", "フォロー状況", "Estado de seguimiento"},
	"occurred_at":      {"Communication time", "沟通时间", "溝通時間", "連絡日時", "Fecha de contacto"},
	"content":          {"Communication", "沟通内容", "溝通內容", "連絡内容", "Contenido"},
	"channel":          {"Channel", "沟通方式", "溝通方式", "連絡手段", "Canal"},
}

func banquetText(i18n *core.I18n, values [5]string) string {
	index := 0
	switch i18n.CurrentLang() {
	case core.LangChinese:
		index = 1
	case core.LangTraditionalChinese:
		index = 2
	case core.LangJapanese:
		index = 3
	case core.LangSpanish:
		index = 4
	}
	return values[index]
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
		sections = append(sections, fmt.Sprintf("- **%s**: %s → %s", banquetText(i18n, label),
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
			lines = append(lines, fmt.Sprintf("- **%s**: %s", banquetText(i18n, banquetLabels[key]), displayField(field, value, i18n)))
		}
	}
	if link := safeLink(row["url"]); link != "" {
		lines = append(lines, "["+banquetText(i18n, [5]string{"Open record", "打开记录", "開啟記錄", "記録を開く", "Abrir registro"})+"]("+link+")")
	}
	return strings.Join(lines, "\n")
}

func renderBanquetReceipt(row map[string]any, i18n *core.I18n) string {
	var lines []string
	for _, key := range []string{"number", "name", "owner", "stage", "banquet_date", "date_status", "date_note", "session", "venue", "estimated_tables", "budget", "visit_at", "visited_at", "next_action", "next_followup_at", "followup_status"} {
		if value, ok := row[key]; ok {
			lines = append(lines, fmt.Sprintf("- **%s**: %s", banquetText(i18n, banquetLabels[key]), displayField(key, value, i18n)))
		}
	}
	if link := safeLink(row["url"]); link != "" {
		lines = append(lines, "["+banquetText(i18n, [5]string{"Open record", "打开记录", "開啟記錄", "記録を開く", "Abrir registro"})+"]("+link+")")
	}
	return strings.Join(lines, "\n")
}
