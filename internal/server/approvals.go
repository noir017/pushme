package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/noir017/pushme/internal/approval"
	"github.com/noir017/pushme/internal/inbox"
)

// 审批（手机确认）：调用方发起 → 收件箱里出现一条带 approval 的消息、飞书也提醒一句 →
// 手机 App 拿接收方 token 取内容、签名答复 → 发起方长轮询取回结果，自己验签。
// pushme 不验签：签名是给发起方验的，pushme 被攻破也伪造不出批准。

const (
	// 标题、正文原样参与签名，超长就拒，不截断（截断了发起方就验不过）。
	maxApprovalTitle = 200
	maxApprovalText  = 4000
)

func (s *Server) approvalsDisabled(w http.ResponseWriter) bool {
	if s.Approvals == nil || len(s.Receivers) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "approvals disabled"})
		return true
	}
	return false
}

func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "missing or invalid bearer token"})
}

// createApproval：POST /approvals，body {"title","text","ttl":秒}，返回 {"id","challenge","expires"}。
func (s *Server) createApproval(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	var in struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		TTL   int    `json:"ttl"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	switch {
	case strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Text) == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "empty approval"})
		return
	case len(in.Title) > maxApprovalTitle || len(in.Text) > maxApprovalText:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "title or text too long (max " +
			strconv.Itoa(maxApprovalTitle) + " / " + strconv.Itoa(maxApprovalText) + " bytes)"})
		return
	case in.TTL < 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ttl must be >= 0"})
		return
	}
	lg := s.log().With("caller", caller)
	if !s.allow(caller) {
		lg.Warn("rate limited")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "rate limited"})
		return
	}
	a, err := s.Approvals.Create(caller, in.Title, in.Text, time.Duration(in.TTL)*time.Second)
	switch {
	case errors.Is(err, approval.ErrTooMany):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": err.Error()})
		return
	case err != nil && a.ID == "":
		lg.Error("create approval failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "create approval failed"})
		return
	case err != nil:
		// 落盘失败：内存里已经有了，照常进行
		lg.Error("approval save failed", "err", err)
	}
	if s.Inbox != nil {
		if _, err := s.Inbox.AddApproval(caller, a.Title, a.Text, inbox.ApprovalRef{ID: a.ID, Expires: a.Expires}); err != nil {
			lg.Error("inbox save failed", "err", err)
		}
	}
	// 飞书只是提醒：批不了，也不等它
	title := "待审批"
	if t := strings.TrimSpace(a.Title); t != "" {
		title += "：" + t
	}
	mins := strconv.FormatInt((a.Expires-a.Created+59)/60, 10)
	go s.sendDetached(caller, format(caller, title, a.Text+"\n\n请在手机 pushme App 上处理，"+mins+" 分钟内有效。"))

	lg.Info("approval created", "id", a.ID, "expires", a.Expires)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": a.ID, "challenge": a.Challenge, "expires": a.Expires})
}

// approvalResult：GET /approvals/{id}?wait=秒。只有发起它的调用方取得到（别人一律 404）。
// 返回 {"state","expires","sig","by","answered"}；还是 pending 时带 wait 最多挂 wait 秒，一有答复立即返回。
func (s *Server) approvalResult(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDisabled(w) {
		return
	}
	caller := s.callerOf(r)
	if caller == "" {
		unauthorized(w)
		return
	}
	id := r.PathValue("id")
	a, ok := s.Approvals.Get(id)
	if !ok || a.Caller != caller {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": approval.ErrNotFound.Error()})
		return
	}
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), maxInboxWait)
	if wait > 0 && a.State == approval.Pending {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(wait+10) * time.Second))
		s.Approvals.Wait(r.Context(), id, time.Duration(wait)*time.Second)
		if a, ok = s.Approvals.Get(id); !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": approval.ErrNotFound.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": a.ID, "state": a.State, "expires": a.Expires,
		"sig": a.Sig, "by": a.By, "answered": a.Answered,
	})
}

// pendingApprovals：GET /approvals，手机取还在等答复的审批（含要签名的 challenge），带服务器时间供校正时钟差。
func (s *Server) pendingApprovals(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDisabled(w) {
		return
	}
	if s.receiverOf(r) == "" {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approvals": s.Approvals.Pending(), "now": time.Now().Unix()})
}

// answerApproval：POST /approvals/{id}，body {"decision":"approve|deny","sig":"base64"}。只认第一次答复。
func (s *Server) answerApproval(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDisabled(w) {
		return
	}
	receiver := s.receiverOf(r)
	if receiver == "" {
		unauthorized(w)
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Sig      string `json:"sig"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	approve := false
	switch in.Decision {
	case "approve":
		if sig, err := base64.StdEncoding.DecodeString(in.Sig); err != nil || len(sig) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "approve needs a base64 sig"})
			return
		}
		approve = true
	case "deny":
		in.Sig = ""
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `decision must be "approve" or "deny"`})
		return
	}
	id := r.PathValue("id")
	a, err := s.Approvals.Answer(id, receiver, approve, in.Sig)
	switch {
	case errors.Is(err, approval.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	case errors.Is(err, approval.ErrNotPending):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error(), "state": a.State})
		return
	case err != nil:
		s.log().Error("approval save failed", "id", id, "err", err)
	}
	s.log().Info("approval answered", "id", id, "caller", a.Caller, "state", a.State, "by", receiver)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": a.ID, "state": a.State})
}
