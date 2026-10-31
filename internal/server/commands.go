package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/noir017/pushme/internal/command"
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
