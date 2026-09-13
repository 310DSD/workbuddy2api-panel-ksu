// school.go 开学季管家：签到排程末尾自动「分享上报 → 领奖 → 抽奖」。
//
// 活动期 2026-09-13 ~ 09-24（每日刷新）：share_invite 判据为纯前端上报
//（POST /tasks/share-complete，实测三账号即点亮），+100c + 1 次抽奖/天/号。
// chat_3_times / expert_use 判据绑定小程序原生沙箱会话，纯 API 不做（需人工）。
// 活动结束后 in_period=false 自动跳过，无需下线代码。
package scheduler

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// schoolPollLoops/LGap share-complete 后的异步计分轮询（实测 2.5s 内点亮）。
const (
	schoolPollLoops = 3
	schoolPollGap   = 2500 * time.Millisecond
)

// RunSchoolNow 对所有可用账号执行开学季活动闭环（幂等：不在期/已领静默跳过）。
// 由 RunCheckinNow 末尾调用（活动是每日刷新，搭每日签到的车最自然）。
func (s *Scheduler) RunSchoolNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		s.schoolAccount(a)
		time.Sleep(activityAccountDelay)
	}
}

// schoolAccount 单账号闭环：查任务 → share-complete → 轮询 → 领奖 → 抽完次数。
func (s *Scheduler) schoolAccount(a *auth.Auth) {
	tasks, inPeriod, err := s.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		log.Printf("school %s: tasks: %v", a.UID, err)
		return
	}
	if !inPeriod {
		return // 活动已结束，静默
	}
	share := findSchoolTask(tasks, "share_invite")
	if share == nil || share.Status == "claimed" {
		return
	}
	if err := s.cfg.Upstream.SchoolShareComplete(a); err != nil {
		log.Printf("school %s: share-complete: %v", a.UID, err)
		return
	}
	// 异步计分轮询。
	done := false
	for i := 0; i < schoolPollLoops && !done; i++ {
		time.Sleep(schoolPollGap)
		tasks2, _, err := s.cfg.Upstream.SchoolTasks(a)
		if err != nil {
			continue
		}
		if t := findSchoolTask(tasks2, "share_invite"); t != nil {
			done = t.Progress >= t.TargetCount && t.TargetCount > 0
		}
	}
	if !done {
		log.Printf("school %s: share-complete 上报后未点亮（明日重试）", a.UID)
		return
	}
	granted, err := s.cfg.Upstream.SchoolClaimTask(a, "share_invite")
	if err != nil {
		log.Printf("school %s: claim: %v", a.UID, err)
		return
	}
	log.Printf("school %s: ★ 分享任务完成，+100c +%d 抽奖次数", a.UID, granted)
	// 抽奖：把余额全抽完（含本次活动新领的次数）。
	chances, err := s.cfg.Upstream.SchoolChances(a)
	if err != nil {
		return
	}
	for i := 0; i < chances; i++ {
		prize, err := s.cfg.Upstream.SchoolDraw(a)
		if err != nil {
			log.Printf("school %s: draw: %v", a.UID, err)
			return
		}
		log.Printf("school %s: 🎲 %s", a.UID, prize)
		time.Sleep(2 * time.Second)
	}
}

// findSchoolTask 按任务码查条目。
func findSchoolTask(tasks []upstream.SchoolTask, code string) *upstream.SchoolTask {
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i]
		}
	}
	return nil
}
