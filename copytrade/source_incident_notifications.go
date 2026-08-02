package copytrade

import (
	"fmt"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

const sourceIncidentNotificationQueueSize = 64

type sourceIncidentNotificationJob struct {
	engine   *Engine
	incident *store.CopyTradeSourceIncident
	kind     string
	health   *store.CopyTradeSourceHealth
	now      time.Time
}

// sourceIncidentNotificationDispatcher is process-scoped because credential
// incidents are shared by every Binance Smart Money engine. Its only purpose is
// to keep member resolution and notifier delivery out of source snapshot and
// signal processing goroutines.
type sourceIncidentNotificationDispatcher struct {
	jobs     chan sourceIncidentNotificationJob
	failures chan sourceIncidentNotificationFailure
}

type sourceIncidentNotificationFailure struct {
	job     sourceIncidentNotificationJob
	message string
}

func newSourceIncidentNotificationDispatcher(capacity int) *sourceIncidentNotificationDispatcher {
	if capacity <= 0 {
		capacity = sourceIncidentNotificationQueueSize
	}
	d := &sourceIncidentNotificationDispatcher{
		jobs:     make(chan sourceIncidentNotificationJob, capacity),
		failures: make(chan sourceIncidentNotificationFailure, capacity),
	}
	go d.run()
	go d.runFailures()
	return d
}

func (d *sourceIncidentNotificationDispatcher) run() {
	for job := range d.jobs {
		if job.engine == nil {
			continue
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					message := fmt.Sprintf("source incident notification panic: %v", recovered)
					logger.Warnf("⚠️ [%s] %s", job.engine.traderID, message)
					select {
					case d.failures <- sourceIncidentNotificationFailure{job: job, message: message}:
					default:
						logger.Warnf("⚠️ [%s] Smart Money 事故邮件失败队列已满，将由持久化 CLAIMED TTL 重试", job.engine.traderID)
					}
				}
			}()
			job.engine.notifySmartMoneySourceIncident(job.incident, job.kind, job.health, job.now)
		}()
	}
}

func (d *sourceIncidentNotificationDispatcher) runFailures() {
	for failure := range d.failures {
		job := failure.job
		if job.engine == nil || job.engine.store == nil || job.incident == nil {
			continue
		}
		if err := job.engine.store.CopyTrade().MarkSourceIncidentMailDelivery(
			job.incident.ID, job.kind, string(notifier.DeliveryDropped), failure.message, job.now,
		); err != nil {
			logger.Warnf("⚠️ [%s] 恢复 Smart Money 事故邮件重试状态失败: %v", job.engine.traderID, err)
		}
	}
}

func (d *sourceIncidentNotificationDispatcher) enqueue(job sourceIncidentNotificationJob) bool {
	select {
	case d.jobs <- job:
		return true
	default:
		return false
	}
}

var smartMoneyIncidentNotificationDispatcher = newSourceIncidentNotificationDispatcher(sourceIncidentNotificationQueueSize)

func (e *Engine) enqueueSmartMoneySourceIncidentNotification(incident *store.CopyTradeSourceIncident, kind string, health *store.CopyTradeSourceHealth, now time.Time) {
	if incident == nil || health == nil || e.store == nil {
		return
	}
	job := sourceIncidentNotificationJob{engine: e, incident: incident, kind: kind, health: health, now: now}
	if smartMoneyIncidentNotificationDispatcher.enqueue(job) {
		return
	}

	message := fmt.Sprintf("source incident notification queue is full (capacity=%d)", sourceIncidentNotificationQueueSize)
	logger.Warnf("⚠️ [%s] %s", e.traderID, message)
	// Persist queue failure on a second bounded worker so a busy database can
	// never turn notification overload into source signal latency. The store
	// maps DROPPED to FAILED with persistent exponential retry. If even this
	// bounded fallback is saturated, the durable CLAIMED TTL remains the final
	// retry safety net.
	select {
	case smartMoneyIncidentNotificationDispatcher.failures <- sourceIncidentNotificationFailure{job: job, message: message}:
	default:
		logger.Warnf("⚠️ [%s] Smart Money 事故邮件失败队列已满，将由持久化 CLAIMED TTL 重试", e.traderID)
	}
}
