package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Key form field indexes.
const (
	keyFieldKey = iota
	keyFieldMaxRequests
	keyFieldMaxTokens
	keyFieldResets
	keyFieldCount
)

// keyTextFieldCount is the number of text inputs in the key form.
const keyTextFieldCount = keyFieldResets

// keyResetCadences lists the selectable reset cadences. The empty string means
// "never" (lifetime) on the wire.
var keyResetCadences = []string{"", "hourly", "daily", "weekly", "monthly"}

// keysTabModel displays and manages API keys.
type keysTabModel struct {
	client       *Client
	viewport     viewport.Model
	entries      []APIKeyEntry
	limits       map[string]APIKeyLimit
	limitsErr    error
	gemini       []map[string]any
	interactions []map[string]any
	claude       []map[string]any
	codex        []map[string]any
	xai          []map[string]any
	vertex       []map[string]any
	openai       []map[string]any
	err          error
	width        int
	height       int
	ready        bool
	cursor       int
	confirm      int // -1 = no deletion pending
	resetConfirm int // -1 = no usage reset pending
	status       string

	// Editing / Adding
	editing bool
	adding  bool
	// editRow is the displayed row being edited; the server index comes from
	// the entry itself.
	editRow     int
	formInputs  [keyTextFieldCount]textinput.Model
	formField   int
	formCadence int
	formErr     string
}

type keysDataMsg struct {
	entries      []APIKeyEntry
	limits       map[string]APIKeyLimit
	limitsErr    error
	gemini       []map[string]any
	interactions []map[string]any
	claude       []map[string]any
	codex        []map[string]any
	xai          []map[string]any
	vertex       []map[string]any
	openai       []map[string]any
	err          error
}

type keyActionMsg struct {
	action string
	err    error
}

func newKeysTabModel(client *Client) keysTabModel {
	m := keysTabModel{
		client:       client,
		confirm:      -1,
		resetConfirm: -1,
	}
	for i := range m.formInputs {
		ti := textinput.New()
		ti.Prompt = ""
		if i == keyFieldKey {
			ti.CharLimit = 512
		} else {
			ti.CharLimit = 24
		}
		m.formInputs[i] = ti
	}
	return m
}

func (m keysTabModel) Init() tea.Cmd {
	return m.fetchKeys
}

func (m keysTabModel) fetchKeys() tea.Msg {
	result := keysDataMsg{}
	entries, err := m.client.GetAPIKeyEntries()
	if err != nil {
		result.err = err
		return result
	}
	result.entries = entries
	limitSnapshots, errLimits := m.client.GetAPIKeyLimits()
	if errLimits != nil {
		result.limitsErr = errLimits
	} else {
		result.limits = make(map[string]APIKeyLimit, len(limitSnapshots))
		for _, snapshot := range limitSnapshots {
			if strings.TrimSpace(snapshot.Key) != "" && snapshot.Limits != nil {
				result.limits[strings.TrimSpace(snapshot.Key)] = *snapshot.Limits
			}
		}
	}
	result.gemini, _ = m.client.GetGeminiKeys()
	result.interactions, _ = m.client.GetInteractionsKeys()
	result.claude, _ = m.client.GetClaudeKeys()
	result.codex, _ = m.client.GetCodexKeys()
	result.xai, _ = m.client.GetXAIKeys()
	result.vertex, _ = m.client.GetVertexKeys()
	result.openai, _ = m.client.GetOpenAICompat()
	return result
}

func (m keysTabModel) Update(msg tea.Msg) (keysTabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case localeChangedMsg:
		m.viewport.SetContent(m.renderContent())
		return m, nil
	case keysDataMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.entries = msg.entries
			m.limits = msg.limits
			m.limitsErr = msg.limitsErr
			m.confirm = -1
			m.resetConfirm = -1
			m.gemini = msg.gemini
			m.interactions = msg.interactions
			m.claude = msg.claude
			m.codex = msg.codex
			m.xai = msg.xai
			m.vertex = msg.vertex
			m.openai = msg.openai
			if m.cursor >= len(m.entries) {
				m.cursor = max(0, len(m.entries)-1)
			}
		}
		m.viewport.SetContent(m.renderContent())
		return m, nil

	case keyActionMsg:
		if msg.err != nil {
			m.status = errorStyle.Render("✗ " + msg.err.Error())
		} else {
			m.status = successStyle.Render("✓ " + msg.action)
		}
		m.confirm = -1
		m.resetConfirm = -1
		m.viewport.SetContent(m.renderContent())
		return m, m.fetchKeys

	case tea.KeyMsg:
		// ---- Editing / Adding mode ----
		if m.editing || m.adding {
			switch msg.String() {
			case "enter":
				return m.submitKeyForm()
			case "esc":
				m.closeKeyForm()
				m.viewport.SetContent(m.renderContent())
				return m, nil
			case "tab", "down":
				m.focusFormField((m.formField + 1) % keyFieldCount)
				m.viewport.SetContent(m.renderContent())
				return m, nil
			case "shift+tab", "up":
				m.focusFormField((m.formField - 1 + keyFieldCount) % keyFieldCount)
				m.viewport.SetContent(m.renderContent())
				return m, nil
			case "left":
				if m.formField == keyFieldResets {
					m.formCadence = (m.formCadence - 1 + len(keyResetCadences)) % len(keyResetCadences)
					m.viewport.SetContent(m.renderContent())
					return m, nil
				}
			case "right":
				if m.formField == keyFieldResets {
					m.formCadence = (m.formCadence + 1) % len(keyResetCadences)
					m.viewport.SetContent(m.renderContent())
					return m, nil
				}
			}
			if m.formField >= keyTextFieldCount {
				m.viewport.SetContent(m.renderContent())
				return m, nil
			}
			var cmd tea.Cmd
			m.formInputs[m.formField], cmd = m.formInputs[m.formField].Update(msg)
			m.viewport.SetContent(m.renderContent())
			return m, cmd
		}

		// ---- Usage reset confirmation ----
		if m.resetConfirm >= 0 {
			switch msg.String() {
			case "y", "Y":
				idx := m.resetConfirm
				m.resetConfirm = -1
				if idx < len(m.entries) {
					key := m.entries[idx].Key
					return m, func() tea.Msg {
						err := m.client.ResetAPIKeyLimits(key)
						if err != nil {
							return keyActionMsg{err: err}
						}
						return keyActionMsg{action: T("key_usage_reset")}
					}
				}
				m.viewport.SetContent(m.renderContent())
				return m, nil
			case "n", "N", "esc":
				m.resetConfirm = -1
				m.viewport.SetContent(m.renderContent())
				return m, nil
			}
			return m, nil
		}

		// ---- Delete confirmation ----
		if m.confirm >= 0 {
			switch msg.String() {
			case "y", "Y":
				row := m.confirm
				m.confirm = -1
				if row >= len(m.entries) {
					m.viewport.SetContent(m.renderContent())
					return m, nil
				}
				// Delete the entry at its original server position, which is not
				// the displayed row once blank entries were filtered out.
				serverIdx := m.entries[row].Index
				return m, func() tea.Msg {
					err := m.client.DeleteAPIKey(serverIdx)
					if err != nil {
						return keyActionMsg{err: err}
					}
					return keyActionMsg{action: T("key_deleted")}
				}
			case "n", "N", "esc":
				m.confirm = -1
				m.viewport.SetContent(m.renderContent())
				return m, nil
			}
			return m, nil
		}

		// ---- Normal mode ----
		switch msg.String() {
		case "j", "down":
			if len(m.entries) > 0 {
				m.cursor = (m.cursor + 1) % len(m.entries)
				m.viewport.SetContent(m.renderContent())
			}
			return m, nil
		case "k", "up":
			if len(m.entries) > 0 {
				m.cursor = (m.cursor - 1 + len(m.entries)) % len(m.entries)
				m.viewport.SetContent(m.renderContent())
			}
			return m, nil
		case "a":
			// Add new key
			m.openKeyForm(true, "", nil)
			m.viewport.SetContent(m.renderContent())
			return m, textinput.Blink
		case "e":
			// Edit selected key with its own configured limits prefilled
			if m.cursor < len(m.entries) {
				entry := m.entries[m.cursor]
				m.editRow = m.cursor
				m.openKeyForm(false, entry.Key, entry.Limits)
				m.viewport.SetContent(m.renderContent())
				return m, textinput.Blink
			}
			return m, nil
		case "d":
			// Delete selected key
			if m.cursor < len(m.entries) {
				m.confirm = m.cursor
				m.viewport.SetContent(m.renderContent())
			}
			return m, nil
		case "x":
			if m.cursor < len(m.entries) {
				if m.effectiveLimit(m.entries[m.cursor]) != nil {
					m.resetConfirm = m.cursor
					m.viewport.SetContent(m.renderContent())
				}
			}
			return m, nil
		case "c":
			// Copy selected key to clipboard
			if m.cursor < len(m.entries) {
				key := m.entries[m.cursor].Key
				if err := clipboard.WriteAll(key); err != nil {
					m.status = errorStyle.Render(T("copy_failed") + ": " + err.Error())
				} else {
					m.status = successStyle.Render(T("copied"))
				}
				m.viewport.SetContent(m.renderContent())
			}
			return m, nil
		case "r":
			m.status = ""
			return m, m.fetchKeys
		default:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *keysTabModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	keyWidth := w - 30
	if keyWidth < 16 {
		keyWidth = 16
	}
	m.formInputs[keyFieldKey].Width = keyWidth
	m.formInputs[keyFieldMaxRequests].Width = 12
	m.formInputs[keyFieldMaxTokens].Width = 12
	if !m.ready {
		m.viewport = viewport.New(w, h)
		m.viewport.SetContent(m.renderContent())
		m.ready = true
	} else {
		m.viewport.Width = w
		m.viewport.Height = h
	}
}

func (m keysTabModel) View() string {
	if !m.ready {
		return T("loading")
	}
	return m.viewport.View()
}

// openKeyForm prepares the add/edit form, prefilling it from the configured
// limits of the key being edited.
func (m *keysTabModel) openKeyForm(adding bool, key string, limits *APIKeyLimitConfig) {
	m.adding = adding
	m.editing = !adding
	m.formErr = ""
	m.formInputs[keyFieldKey].SetValue(key)
	m.formInputs[keyFieldMaxRequests].SetValue("")
	m.formInputs[keyFieldMaxTokens].SetValue("")
	m.formCadence = 0
	if limits != nil {
		if limits.MaxRequests > 0 {
			m.formInputs[keyFieldMaxRequests].SetValue(strconv.FormatInt(limits.MaxRequests, 10))
		}
		if limits.MaxTokensM > 0 {
			m.formInputs[keyFieldMaxTokens].SetValue(strconv.FormatFloat(limits.MaxTokensM, 'f', -1, 64))
		}
		m.formCadence = cadenceIndex(limits.Resets)
	}
	m.focusFormField(keyFieldKey)
}

// closeKeyForm discards the form state.
func (m *keysTabModel) closeKeyForm() {
	m.editing = false
	m.adding = false
	m.formErr = ""
	for i := range m.formInputs {
		m.formInputs[i].Blur()
	}
}

// focusFormField moves the focus to one form field.
func (m *keysTabModel) focusFormField(field int) {
	m.formField = field
	for i := range m.formInputs {
		if i == field {
			m.formInputs[i].Focus()
		} else {
			m.formInputs[i].Blur()
		}
	}
}

// submitKeyForm validates the form and issues the create or update request.
func (m *keysTabModel) submitKeyForm() (keysTabModel, tea.Cmd) {
	key := strings.TrimSpace(m.formInputs[keyFieldKey].Value())
	if key == "" {
		m.formErr = T("key_form_key_required")
		m.viewport.SetContent(m.renderContent())
		return *m, nil
	}
	limits, err := m.formLimits()
	if err != nil {
		m.formErr = err.Error()
		m.viewport.SetContent(m.renderContent())
		return *m, nil
	}
	isAdding := m.adding
	// Edits address the original server position of the entry, not its row.
	serverIdx := -1
	if !isAdding {
		if m.editRow >= len(m.entries) {
			m.formErr = T("key_form_key_required")
			m.viewport.SetContent(m.renderContent())
			return *m, nil
		}
		serverIdx = m.entries[m.editRow].Index
	}
	client := m.client
	m.closeKeyForm()
	m.viewport.SetContent(m.renderContent())
	if isAdding {
		return *m, func() tea.Msg {
			if errAdd := client.AddAPIKey(key, limits); errAdd != nil {
				return keyActionMsg{err: errAdd}
			}
			return keyActionMsg{action: T("key_added")}
		}
	}
	return *m, func() tea.Msg {
		if errEdit := client.EditAPIKey(serverIdx, key, limits); errEdit != nil {
			return keyActionMsg{err: errEdit}
		}
		return keyActionMsg{action: T("key_updated")}
	}
}

// formLimits builds the limit block from the form, returning nil when nothing
// is configured (an unlimited key).
func (m keysTabModel) formLimits() (*APIKeyLimitConfig, error) {
	maxRequests, err := parseLimitInt(m.formInputs[keyFieldMaxRequests].Value())
	if err != nil {
		return nil, err
	}
	maxTokensM, err := parseLimitFloat(m.formInputs[keyFieldMaxTokens].Value())
	if err != nil {
		return nil, err
	}
	cadence := keyResetCadences[m.formCadence]
	if maxRequests == 0 && maxTokensM == 0 {
		if cadence == "" {
			return nil, nil
		}
		// A reset cadence without any cap enforces nothing and the server
		// discards it, so refuse it here instead of sending an inert limit.
		return nil, fmt.Errorf("%s", T("key_form_cadence_needs_cap"))
	}
	return &APIKeyLimitConfig{MaxRequests: maxRequests, MaxTokensM: maxTokensM, Resets: cadence}, nil
}

// parseLimitInt reads an optional non-negative integer limit.
func parseLimitInt(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s", T("key_form_invalid_number"))
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s", T("key_form_negative"))
	}
	return parsed, nil
}

// parseLimitFloat reads an optional non-negative decimal limit.
func parseLimitFloat(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s", T("key_form_invalid_number"))
	}
	// ParseFloat accepts NaN and the infinities; neither is a usable cap.
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s", T("key_form_invalid_number"))
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s", T("key_form_negative"))
	}
	return parsed, nil
}

// cadenceIndex maps a configured cadence to its selector position.
func cadenceIndex(cadence string) int {
	cadence = strings.TrimSpace(cadence)
	if cadence == "lifetime" {
		return 0
	}
	for i, candidate := range keyResetCadences {
		if candidate == cadence {
			return i
		}
	}
	return 0
}

// effectiveLimit merges the limits configured on the entry itself with the
// runtime usage so that a limited key still renders its ceilings when no
// request was recorded yet. Configured limits come from the entry (duplicate
// key strings keep their own block); runtime usage is keyed by key string.
func (m keysTabModel) effectiveLimit(entry APIKeyEntry) *APIKeyLimit {
	runtime, hasRuntime := m.limits[entry.Key]
	config := entry.Limits
	if !hasRuntime && config == nil {
		return nil
	}
	merged := runtime
	if config != nil {
		if config.MaxRequests == 0 && config.MaxTokensM == 0 && strings.TrimSpace(config.Resets) == "" && !hasRuntime {
			return nil
		}
		merged.MaxRequests = config.MaxRequests
		// Round like the backend MaxTokens() so both agree on the token count.
		merged.MaxTokens = int64(math.Round(config.MaxTokensM * 1_000_000))
		if strings.TrimSpace(config.Resets) != "" {
			merged.Resets = config.Resets
		} else if !hasRuntime {
			merged.Resets = "lifetime"
		}
	}
	return &merged
}

// renderKeyForm draws the add/edit form box.
func (m keysTabModel) renderKeyForm() string {
	title := T("key_form_title_edit")
	if m.adding {
		title = T("key_form_title_add")
	}
	cadence := "‹ " + formatAPIKeyLimitCadence(cadenceLabel(keyResetCadences[m.formCadence])) + " ›"
	lines := []string{
		m.renderFormLine(keyFieldKey, T("key_form_key"), m.formInputs[keyFieldKey].View(), ""),
		m.renderFormLine(keyFieldMaxRequests, T("key_form_max_requests"), m.formInputs[keyFieldMaxRequests].View(), T("key_form_blank_unlimited")),
		m.renderFormLine(keyFieldMaxTokens, T("key_form_max_tokens"), m.formInputs[keyFieldMaxTokens].View(), T("key_form_blank_unlimited")),
		m.renderFormLine(keyFieldResets, T("key_form_resets"), cadence, T("key_form_cadences")),
	}
	if m.formErr != "" {
		lines = append(lines, errorStyle.Render("✗ "+m.formErr))
	}

	var sb strings.Builder
	sb.WriteString(renderFormBox(title, lines))
	sb.WriteString(helpStyle.Render("  " + T("key_form_help")))
	sb.WriteString("\n")
	return sb.String()
}

// renderFormLine renders one label/value row, highlighting the focused field.
func (m keysTabModel) renderFormLine(field int, label, value, hint string) string {
	marker := "  "
	style := lipgloss.NewStyle()
	if m.formField == field {
		marker = "▸ "
		style = style.Bold(true).Foreground(colorHighlight)
	}
	line := marker + style.Render(padRight(label, 16)) + value
	if hint != "" {
		line += "  " + helpStyle.Render(hint)
	}
	return line
}

// renderFormBox wraps form lines in a titled rounded box.
func renderFormBox(title string, lines []string) string {
	width := lipgloss.Width(title) + 6
	for _, line := range lines {
		if w := lipgloss.Width(line); w+4 > width {
			width = w + 4
		}
	}
	border := lipgloss.NewStyle().Foreground(colorBorder)
	var sb strings.Builder
	sb.WriteString("  ")
	sb.WriteString(border.Render("┌─ "+title+" ") + border.Render(strings.Repeat("─", width-lipgloss.Width(title)-5)+"┐"))
	sb.WriteString("\n")
	for _, line := range lines {
		sb.WriteString("  " + border.Render("│") + " " + line + strings.Repeat(" ", width-lipgloss.Width(line)-3) + border.Render("│"))
		sb.WriteString("\n")
	}
	sb.WriteString("  " + border.Render("└"+strings.Repeat("─", width-2)+"┘"))
	sb.WriteString("\n")
	return sb.String()
}

// padRight pads a label to a fixed display width.
func padRight(value string, width int) string {
	if pad := width - lipgloss.Width(value); pad > 0 {
		return value + strings.Repeat(" ", pad)
	}
	return value
}

// cadenceLabel maps the empty cadence to the "never" translation key input.
func cadenceLabel(cadence string) string {
	if cadence == "" {
		return "never"
	}
	return cadence
}

func (m keysTabModel) renderContent() string {
	var sb strings.Builder

	sb.WriteString(titleStyle.Render(T("keys_title")))
	sb.WriteString("\n")
	sb.WriteString(helpStyle.Render(T("keys_help")))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(errorStyle.Render(T("error_prefix") + m.err.Error()))
		sb.WriteString("\n")
		return sb.String()
	}

	// ━━━ Access API Keys (interactive) ━━━
	sb.WriteString(tableHeaderStyle.Render(fmt.Sprintf("  %s (%d)", T("access_keys"), len(m.entries))))
	sb.WriteString("\n")
	if m.limitsErr != nil {
		sb.WriteString(warningStyle.Render(fmt.Sprintf("  "+T("limits_unavailable"), m.limitsErr)))
		sb.WriteString("\n")
	}

	if len(m.entries) == 0 {
		sb.WriteString(subtitleStyle.Render(T("no_keys")))
		sb.WriteString("\n")
	}

	for i, entry := range m.entries {
		key := entry.Key
		cursor := "  "
		rowStyle := lipgloss.NewStyle()
		if i == m.cursor {
			cursor = "▸ "
			rowStyle = lipgloss.NewStyle().Bold(true)
		}

		row := fmt.Sprintf("%s%d. %s", cursor, i+1, maskKey(key))
		if limits := m.effectiveLimit(entry); limits != nil {
			row += "  " + formatAPIKeyLimit(*limits)
		} else {
			row += "  " + T("unlimited")
		}
		sb.WriteString(rowStyle.Render(row))
		sb.WriteString("\n")

		// Delete confirmation
		if m.confirm == i {
			sb.WriteString(warningStyle.Render(fmt.Sprintf("    "+T("confirm_delete_key"), maskKey(key))))
			sb.WriteString("\n")
		}
		if m.resetConfirm == i {
			sb.WriteString(warningStyle.Render(fmt.Sprintf("    "+T("confirm_reset_key_usage"), maskKey(key))))
			sb.WriteString("\n")
		}

		// Edit form
		if m.editing && m.editRow == i {
			sb.WriteString(m.renderKeyForm())
		}
	}

	// Add form
	if m.adding {
		sb.WriteString("\n")
		sb.WriteString(m.renderKeyForm())
	}

	sb.WriteString("\n")

	// ━━━ Provider Keys (read-only display) ━━━
	renderProviderKeys(&sb, "Gemini API Keys", m.gemini)
	renderProviderKeys(&sb, "Interactions API Keys", m.interactions)
	renderProviderKeys(&sb, "Claude API Keys", m.claude)
	renderProviderKeys(&sb, "Codex API Keys", m.codex)
	renderProviderKeys(&sb, "xAI API Keys", m.xai)
	renderProviderKeys(&sb, "Vertex API Keys", m.vertex)

	if len(m.openai) > 0 {
		renderSection(&sb, "OpenAI Compatibility", len(m.openai))
		for i, entry := range m.openai {
			name := getString(entry, "name")
			baseURL := getString(entry, "base-url")
			prefix := getString(entry, "prefix")
			info := name
			if prefix != "" {
				info += " (prefix: " + prefix + ")"
			}
			if baseURL != "" {
				info += " → " + baseURL
			}
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, info))
		}
		sb.WriteString("\n")
	}

	if m.status != "" {
		sb.WriteString(m.status)
		sb.WriteString("\n")
	}

	return sb.String()
}

func renderSection(sb *strings.Builder, title string, count int) {
	header := fmt.Sprintf("%s (%d)", title, count)
	sb.WriteString(tableHeaderStyle.Render("  " + header))
	sb.WriteString("\n")
}

func renderProviderKeys(sb *strings.Builder, title string, keys []map[string]any) {
	if len(keys) == 0 {
		return
	}
	renderSection(sb, title, len(keys))
	for i, key := range keys {
		apiKey := getString(key, "api-key")
		prefix := getString(key, "prefix")
		baseURL := getString(key, "base-url")
		info := maskKey(apiKey)
		if prefix != "" {
			info += " (prefix: " + prefix + ")"
		}
		if baseURL != "" {
			info += " → " + baseURL
		}
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, info))
	}
	sb.WriteString("\n")
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

func formatAPIKeyLimit(limits APIKeyLimit) string {
	resetAt := T("limit_never")
	switch {
	case limits.ResetAt != nil:
		resetAt = limits.ResetAt.Format(time.RFC3339)
	case limits.Resets != "" && limits.Resets != "lifetime":
		// A windowed key that has not been used yet has no open window, so the
		// tracker reports no reset time. Showing "never" would contradict the
		// cadence printed next to it.
		resetAt = T("limit_reset_pending")
	}
	return fmt.Sprintf(
		T("key_limit_usage"),
		formatAPIKeyLimitValue(limits.RequestsUsed, limits.MaxRequests),
		formatAPIKeyLimitValue(limits.TokensUsed, limits.MaxTokens),
		formatAPIKeyLimitCadence(limits.Resets),
		resetAt,
	)
}

func formatAPIKeyLimitValue(used, limit int64) string {
	limitText := T("unlimited")
	if limit > 0 {
		limitText = strconv.FormatInt(limit, 10)
	}
	return strconv.FormatInt(used, 10) + "/" + limitText
}

func formatAPIKeyLimitCadence(cadence string) string {
	switch cadence {
	case "hourly", "daily", "weekly", "monthly", "lifetime", "never":
		return T("limit_" + cadence)
	default:
		return cadence
	}
}
