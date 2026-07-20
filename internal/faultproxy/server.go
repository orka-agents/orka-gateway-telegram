package faultproxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBotID               = int64(987654321)
	defaultRetryAfterSeconds   = 1
	defaultDelay               = time.Second
	defaultMaxRequestBodyBytes = int64(1 << 20)
	defaultAuditCapacity       = 1024
	telegramGetMeMethod        = "getMe"
	telegramSetWebhookMethod   = "setWebhook"
	telegramSendMessageMethod  = "sendMessage"
)

// Server is a concurrent-safe Telegram Bot API fault simulator.
type Server struct {
	controlTokenHash    [sha256.Size]byte
	botID               int64
	defaultRetryAfter   int
	defaultDelay        time.Duration
	maxRequestBodyBytes int64
	auditCapacity       int
	now                 func() time.Time

	mu           sync.Mutex
	plan         []Action
	audit        []AuditEntry
	nextSequence uint64
}

// New constructs a fault simulator. The control token is never retained in
// plaintext or included in returned errors.
func New(config Config) (*Server, error) {
	controlToken := strings.TrimSpace(config.ControlToken)
	if controlToken == "" {
		return nil, errors.New("fault proxy control token is required")
	}
	if strings.IndexFunc(controlToken, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return nil, errors.New("fault proxy control token must not contain whitespace")
	}

	botID := config.BotID
	if botID == 0 {
		botID = defaultBotID
	}
	if botID < 0 {
		return nil, errors.New("fault proxy bot ID must be positive")
	}
	retryAfter := config.DefaultRetryAfterSeconds
	if retryAfter == 0 {
		retryAfter = defaultRetryAfterSeconds
	}
	if retryAfter < 0 {
		return nil, errors.New("fault proxy default retry_after must be positive")
	}
	if int64(retryAfter) > maxRetryAfterSeconds {
		return nil, errors.New("fault proxy default retry_after exceeds the supported duration")
	}
	delay := config.DefaultDelay
	if delay == 0 {
		delay = defaultDelay
	}
	if delay < 0 {
		return nil, errors.New("fault proxy default delay must be positive")
	}
	maxRequestBodyBytes := config.MaxRequestBodyBytes
	if maxRequestBodyBytes == 0 {
		maxRequestBodyBytes = defaultMaxRequestBodyBytes
	}
	if maxRequestBodyBytes < 0 {
		return nil, errors.New("fault proxy request body limit must be positive")
	}
	auditCapacity := config.AuditCapacity
	if auditCapacity == 0 {
		auditCapacity = defaultAuditCapacity
	}
	if auditCapacity < 0 {
		return nil, errors.New("fault proxy audit capacity must be positive")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	return &Server{
		controlTokenHash:    sha256.Sum256([]byte(controlToken)),
		botID:               botID,
		defaultRetryAfter:   retryAfter,
		defaultDelay:        delay,
		maxRequestBodyBytes: maxRequestBodyBytes,
		auditCapacity:       auditCapacity,
		now:                 now,
	}, nil
}

// Handler returns the HTTP handler for health, Telegram, and control routes.
func (s *Server) Handler() http.Handler {
	return s
}

// ReplacePlan atomically replaces the remaining one-shot FIFO action plan.
func (s *Server) ReplacePlan(actions []Action) error {
	plan := append([]Action(nil), actions...)
	for _, action := range plan {
		if err := action.Validate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.plan = plan
	s.mu.Unlock()
	return nil
}

// ResetPlan removes all pending actions without clearing the audit log.
func (s *Server) ResetPlan() {
	s.mu.Lock()
	s.plan = nil
	s.mu.Unlock()
}

// State returns a defensive snapshot containing no API token or full text.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	pendingActions := make([]Action, len(s.plan))
	copy(pendingActions, s.plan)
	audit := make([]AuditEntry, len(s.audit))
	copy(audit, s.audit)
	return State{PendingActions: pendingActions, Audit: audit}
}

// ServeHTTP dispatches exact public/control paths and Telegram-style bot paths
// without retaining or echoing the token-bearing request path.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case HealthPath, ReadyPath:
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet, false)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "ok\n")
		return
	case ControlPlanPath, ControlStatePath:
		s.serveControl(writer, request)
		return
	default:
		s.serveTelegram(writer, request)
	}
}

func (s *Server) serveControl(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !s.authorized(request) {
		writeControlError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch request.URL.Path {
	case ControlStatePath:
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet, true)
			return
		}
		writeJSON(writer, http.StatusOK, s.State())
	case ControlPlanPath:
		switch request.Method {
		case http.MethodPut, http.MethodPost:
			body, ok := readBoundedBody(writer, request, s.maxRequestBodyBytes, true)
			if !ok {
				return
			}
			actions, err := decodePlan(body)
			if err != nil {
				writeControlError(writer, http.StatusBadRequest, "invalid action plan")
				return
			}
			if err := s.ReplacePlan(actions); err != nil {
				writeControlError(writer, http.StatusBadRequest, "invalid action plan")
				return
			}
			writeJSON(writer, http.StatusOK, struct {
				PendingActions int `json:"pending_actions"`
			}{PendingActions: len(actions)})
		case http.MethodDelete:
			s.ResetPlan()
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.Header().Set("Allow", strings.Join([]string{http.MethodPut, http.MethodPost, http.MethodDelete}, ", "))
			writeControlError(writer, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func (s *Server) authorized(request *http.Request) bool {
	fields := strings.Fields(request.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return false
	}
	providedHash := sha256.Sum256([]byte(fields[1]))
	return subtle.ConstantTimeCompare(providedHash[:], s.controlTokenHash[:]) == 1
}

func (s *Server) serveTelegram(writer http.ResponseWriter, request *http.Request) {
	method, ok := telegramMethod(request.URL.Path)
	if !ok {
		writeTelegramError(writer, http.StatusNotFound, "Not Found", 0)
		return
	}

	switch method {
	case telegramGetMeMethod:
		if request.Method != http.MethodGet && request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost, false)
			return
		}
		if request.Method == http.MethodPost {
			if _, ok := readBoundedBody(writer, request, s.maxRequestBodyBytes, false); !ok {
				return
			}
		}
		writeJSON(writer, http.StatusOK, telegramEnvelope[telegramUser]{
			OK: true,
			Result: telegramUser{
				ID:             s.botID,
				IsBot:          true,
				FirstName:      "Fault Proxy",
				Username:       "telegram_fault_proxy_bot",
				CanJoinGroups:  true,
				SupportsInline: false,
			},
		})
	case telegramSetWebhookMethod:
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost, false)
			return
		}
		if _, ok := readBoundedBody(writer, request, s.maxRequestBodyBytes, false); !ok {
			return
		}
		writeJSON(writer, http.StatusOK, telegramEnvelope[bool]{OK: true, Result: true})
	case telegramSendMessageMethod:
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost, false)
			return
		}
		s.serveSendMessage(writer, request)
	default:
		writeTelegramError(writer, http.StatusNotFound, "Not Found", 0)
	}
}

func telegramMethod(requestPath string) (string, bool) {
	const prefix = "/bot"
	if !strings.HasPrefix(requestPath, prefix) {
		return "", false
	}
	remainder := requestPath[len(prefix):]
	separator := strings.IndexByte(remainder, '/')
	if separator <= 0 || separator == len(remainder)-1 {
		return "", false
	}
	method := remainder[separator+1:]
	if strings.ContainsRune(method, '/') {
		return "", false
	}
	return method, true
}

type sendMessageRequest struct {
	ChatID          int64  `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	Text            string `json:"text"`
	ReplyParameters *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_parameters,omitempty"`
}

type execution struct {
	Action    Action
	Sequence  uint64
	Timestamp time.Time
}

func (s *Server) serveSendMessage(writer http.ResponseWriter, request *http.Request) {
	body, ok := readBoundedBody(writer, request, s.maxRequestBodyBytes, false)
	if !ok {
		return
	}
	var sendRequest sendMessageRequest
	if err := json.Unmarshal(body, &sendRequest); err != nil || sendRequest.ChatID == 0 || sendRequest.Text == "" || sendRequest.MessageThreadID < 0 ||
		(sendRequest.ReplyParameters != nil && sendRequest.ReplyParameters.MessageID < 0) {
		writeTelegramError(writer, http.StatusBadRequest, "Bad Request: invalid request", 0)
		return
	}

	execution := s.consume(sendRequest)
	switch execution.Action.Kind {
	case ActionSuccess:
		s.writeMessageSuccess(writer, sendRequest, execution, sendRequest.ChatID)
	case ActionRateLimit:
		retryAfter := execution.Action.RetryAfterSeconds
		if retryAfter == 0 {
			retryAfter = s.defaultRetryAfter
		}
		writeTelegramError(writer, http.StatusTooManyRequests, "Too Many Requests: retry after "+strconv.Itoa(retryAfter), retryAfter)
	case ActionServerError:
		writeTelegramError(writer, http.StatusInternalServerError, "Internal Server Error", 0)
	case ActionBadRequest:
		writeTelegramError(writer, http.StatusBadRequest, "Bad Request: chat not found", 0)
	case ActionForbidden:
		writeTelegramError(writer, http.StatusForbidden, "Forbidden: bot was blocked by the user", 0)
	case ActionMalformedSuccess:
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "not-json\n")
	case ActionMismatchedSuccess:
		s.writeMessageSuccess(writer, sendRequest, execution, mismatchedChatID(sendRequest.ChatID))
	case ActionDropAfterRead:
		dropConnection(writer)
	case ActionDelayedSuccess:
		delay := time.Duration(execution.Action.DelayMilliseconds) * time.Millisecond
		if delay == 0 {
			delay = s.defaultDelay
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			s.writeMessageSuccess(writer, sendRequest, execution, sendRequest.ChatID)
		case <-request.Context().Done():
			return
		}
	default:
		writeTelegramError(writer, http.StatusInternalServerError, "Internal Server Error", 0)
	}
}

func (s *Server) consume(request sendMessageRequest) execution {
	textHash := sha256.Sum256([]byte(request.Text))
	replyTarget := ReplyTarget{MessageThreadID: request.MessageThreadID}
	if request.ReplyParameters != nil {
		replyTarget.MessageID = request.ReplyParameters.MessageID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	action := Action{Kind: ActionSuccess}
	if len(s.plan) > 0 {
		action = s.plan[0]
		s.plan[0] = Action{}
		s.plan = s.plan[1:]
		if len(s.plan) == 0 {
			s.plan = nil
		}
	}
	s.nextSequence++
	timestamp := s.now().UTC()
	entry := AuditEntry{
		Sequence:    s.nextSequence,
		Action:      action.Kind,
		ChatID:      request.ChatID,
		TextLength:  len(request.Text),
		TextSHA256:  hex.EncodeToString(textHash[:]),
		ReplyTarget: replyTarget,
		Timestamp:   timestamp,
	}
	if len(s.audit) < s.auditCapacity {
		s.audit = append(s.audit, entry)
	} else {
		copy(s.audit, s.audit[1:])
		s.audit[len(s.audit)-1] = entry
	}
	return execution{Action: action, Sequence: s.nextSequence, Timestamp: timestamp}
}

func (s *Server) writeMessageSuccess(writer http.ResponseWriter, request sendMessageRequest, execution execution, chatID int64) {
	messageID := int64(execution.Sequence)
	if messageID <= 0 {
		messageID = math.MaxInt64
	}
	chat := telegramChat{ID: chatID, Type: chatType(chatID)}
	message := telegramMessage{
		MessageID:       messageID,
		Date:            execution.Timestamp.Unix(),
		Chat:            chat,
		MessageThreadID: request.MessageThreadID,
		Text:            request.Text,
	}
	if request.ReplyParameters != nil && request.ReplyParameters.MessageID > 0 {
		message.ReplyToMessage = &telegramReplyMessage{
			MessageID: request.ReplyParameters.MessageID,
			Date:      execution.Timestamp.Unix(),
			Chat:      chat,
		}
	}
	writeJSON(writer, http.StatusOK, telegramEnvelope[telegramMessage]{OK: true, Result: message})
}

func mismatchedChatID(chatID int64) int64 {
	if chatID == math.MaxInt64 {
		return chatID - 1
	}
	mismatch := chatID + 1
	if mismatch == 0 {
		return chatID - 1
	}
	return mismatch
}

func chatType(chatID int64) string {
	if chatID < 0 {
		return "supergroup"
	}
	return "private"
}

func dropConnection(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = connection.Close()
}

func decodePlan(body []byte) ([]Action, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("action plan is empty")
	}
	if body[0] == '[' {
		var actions []Action
		if err := decodeOneJSON(body, &actions, true); err != nil {
			return nil, err
		}
		return actions, nil
	}
	var request struct {
		Actions *[]Action `json:"actions"`
	}
	if err := decodeOneJSON(body, &request, true); err != nil {
		return nil, err
	}
	if request.Actions == nil {
		return nil, errors.New("actions are required")
	}
	return *request.Actions, nil
}

func decodeOneJSON(body []byte, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func readBoundedBody(writer http.ResponseWriter, request *http.Request, limit int64, control bool) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, limit))
	if err == nil {
		return body, true
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		if control {
			writeControlError(writer, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeTelegramError(writer, http.StatusRequestEntityTooLarge, "Request Entity Too Large", 0)
		}
		return nil, false
	}
	if control {
		writeControlError(writer, http.StatusBadRequest, "request body could not be read")
	} else {
		writeTelegramError(writer, http.StatusBadRequest, "Bad Request: invalid request", 0)
	}
	return nil, false
}

type telegramEnvelope[T any] struct {
	OK     bool `json:"ok"`
	Result T    `json:"result"`
}

type telegramUser struct {
	ID             int64  `json:"id"`
	IsBot          bool   `json:"is_bot"`
	FirstName      string `json:"first_name"`
	Username       string `json:"username"`
	CanJoinGroups  bool   `json:"can_join_groups"`
	SupportsInline bool   `json:"supports_inline_queries"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramReplyMessage struct {
	MessageID int64        `json:"message_id"`
	Date      int64        `json:"date"`
	Chat      telegramChat `json:"chat"`
}

type telegramMessage struct {
	MessageID       int64                 `json:"message_id"`
	Date            int64                 `json:"date"`
	Chat            telegramChat          `json:"chat"`
	MessageThreadID int64                 `json:"message_thread_id,omitempty"`
	Text            string                `json:"text"`
	ReplyToMessage  *telegramReplyMessage `json:"reply_to_message,omitempty"`
}

type telegramErrorEnvelope struct {
	OK          bool                        `json:"ok"`
	ErrorCode   int                         `json:"error_code"`
	Description string                      `json:"description"`
	Parameters  *telegramResponseParameters `json:"parameters,omitempty"`
}

type telegramResponseParameters struct {
	RetryAfter int `json:"retry_after,omitempty"`
}

func writeTelegramError(writer http.ResponseWriter, status int, description string, retryAfter int) {
	response := telegramErrorEnvelope{OK: false, ErrorCode: status, Description: description}
	if retryAfter > 0 {
		response.Parameters = &telegramResponseParameters{RetryAfter: retryAfter}
	}
	writeJSON(writer, status, response)
}

func writeControlError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func methodNotAllowed(writer http.ResponseWriter, allow string, control bool) {
	writer.Header().Set("Allow", allow)
	if control {
		writeControlError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeTelegramError(writer, http.StatusMethodNotAllowed, "Method Not Allowed", 0)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
