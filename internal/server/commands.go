package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/noir017/pushme/internal/command"
	"github.com/noir017/pushme/internal/inbox"
)

// 指令信箱：手机（接收方）给某个调用方投签好名的指令，调用方（宿主上的 pushme listen）长轮询取走、自己验签执行。
// pushme 不验签。和审批一样，配了接收方才开。

// listenerFresh 内来取过指令的调用方才算在线，App 只列这些。
const listenerFresh = 3 * time.Minute

func (s *Server) commandsDisabled(w http.ResponseWriter) bool {
	if s.Commands == nil || len(s.Receivers) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "commands disabled"})
		return true
	}
	return false
}

func (s *Server) isCaller(name string) bool {
	for _, n := range s.Tokens {
		if n == name {
			return true
		}
	}
	return false
}

// postCommand：POST /commands（接收方），body {"to","id","issued","expires","body","sig"}。
func (s *Server) postCommand(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	receiver := s.receiverOf(r)
	if receiver == "" {
		unauthorized(w)
		return
	}
	var c command.Command
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	if err := command.Check(c, time.Now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if sig, err := base64.StdEncoding.DecodeString(c.Sig); err != nil || len(sig) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "sig must be base64"})
		return
	}
	if !s.isCaller(c.To) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no such caller: " + c.To})
		return
	}
	c.By = receiver
	c = s.Commands.Add(c)
	s.log().Info("command queued", "to", c.To, "id", c.ID, "seq", c.Seq, "by", receiver)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": c.Seq})
}

// getCommands：GET /commands?after=&wait=&label=（调用方），取发给自己的指令；顺带登记自己在线。
// after 缺省或为负：不回放，立即返回当前 cursor。
func (s *Server) getCommands(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	q := r.URL.Query()
	label := q.Get("label")
	if len(label) > 40 {
		label = label[:40]
	}
	s.Commands.Seen(caller, label)
	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commands": []command.Command{}, "cursor": s.Commands.Latest()})
		return
	}
	wait, _ := strconv.Atoi(q.Get("wait"))
	wait = min(max(wait, 0), maxInboxWait)
	if wait > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(wait+10) * time.Second))
		s.Commands.Wait(r.Context(), caller, after, time.Duration(wait)*time.Second)
		s.Commands.Seen(caller, label) // 挂了一阵，再记一次
	}
	cmds, cursor := s.Commands.After(caller, after)
	if len(cmds) > 0 {
		s.log().Info("commands delivered", "to", caller, "count", len(cmds), "cursor", cursor)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commands": cmds, "cursor": cursor})
}

// commandStatus：POST /commands/status（调用方），body 是一个 JSON 对象（≤4KB），作为本调用方的当前状态挂在在线名单上，
// App 从 GET /commands/targets 读。pushme 不解析内容，原样转发；只放内存，重启就丢（listen 会定时重报）。
func (s *Server) commandStatus(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, command.MaxStatus+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read body: " + err.Error()})
		return
	}
	if len(body) > command.MaxStatus {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "status must be at most " + strconv.Itoa(command.MaxStatus) + " bytes"})
		return
	}
	var obj map[string]json.RawMessage
	var compact bytes.Buffer
	if json.Unmarshal(body, &obj) != nil || obj == nil || json.Compact(&compact, body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "status must be a JSON object"})
		return
	}
	s.Commands.Report(caller, compact.Bytes())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// commandTargets：GET /commands/targets（接收方），最近在线、能下指令的调用方，带服务器时间供手机校正。
func (s *Server) commandTargets(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	if s.receiverOf(r) == "" {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "targets": s.Commands.Listeners(listenerFresh), "now": time.Now().Unix()})
}

// ---- 提权申请：agent 提、App 批（批准 = App 另签一条授权指令发给 target，pushme 只记结果）----

// createRequest：POST /commands/requests（调用方），body {"target","hosts","duration","reason"}，返回 {"id","expires"}。
func (s *Server) createRequest(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	var in command.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	req := command.Request{Caller: caller, Target: in.Target, Hosts: in.Hosts, Duration: in.Duration, Reason: in.Reason}
	if err := command.CheckRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !s.isCaller(req.Target) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no such caller: " + req.Target})
		return
	}
	lg := s.log().With("caller", caller)
	if !s.allow(caller) {
		lg.Warn("rate limited")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "rate limited"})
		return
	}
	req, err := s.Commands.AddRequest(req)
	if err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	title := caller + " 申请提权：" + req.Hosts + " " + req.Duration
	if s.Inbox != nil {
		if _, err := s.Inbox.AddRequest(caller, title, req.Reason, inbox.ApprovalRef{ID: req.ID, Expires: req.Expires}); err != nil {
			lg.Error("inbox save failed", "err", err)
		}
	}
	go s.sendDetached(caller, format(caller, title, "理由："+req.Reason+"\n\n请在手机 pushme App 上处理，5 分钟内有效。"))
	lg.Info("grant request created", "id", req.ID, "target", req.Target, "hosts", req.Hosts, "duration", req.Duration)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": req.ID, "expires": req.Expires})
}

// pendingRequests：GET /commands/requests（接收方），还在等处理的申请，带服务器时间。
func (s *Server) pendingRequests(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	if s.receiverOf(r) == "" {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "requests": s.Commands.PendingRequests(), "now": time.Now().Unix()})
}

// answerRequest：POST /commands/requests/{id}（接收方），body {"decision":"approve"|"deny"}。只认第一次，之后或过期了 409。
// 批准只是记账：真正的授权是 App 签好、发给 target 的那条指令。
func (s *Server) answerRequest(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	receiver := s.receiverOf(r)
	if receiver == "" {
		unauthorized(w)
		return
	}
	var in struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || (in.Decision != "approve" && in.Decision != "deny") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `decision must be "approve" or "deny"`})
		return
	}
	req, err := s.Commands.AnswerRequest(r.PathValue("id"), in.Decision == "approve", receiver)
	switch {
	case errors.Is(err, command.ErrNoRequest):
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error(), "state": req.State})
		return
	}
	s.log().Info("grant request answered", "id", req.ID, "caller", req.Caller, "state", req.State, "by", receiver)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": req.State})
}

// requestResult：GET /commands/requests/{id}?wait=秒（发申请的调用方），返回 {"state","expires","answered"}；
// pending 时最多挂 wait 秒，一有结果立即返回。别的调用方取不到（404）。
func (s *Server) requestResult(w http.ResponseWriter, r *http.Request) {
	if s.commandsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	id := r.PathValue("id")
	req, ok := s.Commands.GetRequest(id)
	if !ok || req.Caller != caller {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": command.ErrNoRequest.Error()})
		return
	}
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), maxInboxWait)
	if wait > 0 && req.State == command.RequestPending {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(wait+10) * time.Second))
		s.Commands.WaitRequest(r.Context(), id, time.Duration(wait)*time.Second)
		if req, ok = s.Commands.GetRequest(id); !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": command.ErrNoRequest.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": req.State, "expires": req.Expires, "answered": req.Answered})
}
