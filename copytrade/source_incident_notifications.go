package copytrade

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"nofx/logger"
	"nofx/notifier"
	"nofx/store"
)

const sourceIncidentNotificationQueueSize = 64

const sourceIncidentNotificationDBTimeout = time.Second

type sourceIncidentObservationJob struct {
	engine      *Engine
	observation store.SourceIncidentObservation
	health      *store.CopyTradeSourceHealth
}

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
	observations chan sourceIncidentObservationJob
	jobs         chan sourceIncidentNotificationJob
	failures     chan sourceIncidentNotificationFailure
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
		observations: make(chan sourceIncidentObservationJob, capacity),
		jobs:         make(chan sourceIncidentNotificationJob, capacity),
		failures:     make(chan sourceIncidentNotificationFailure, capacity),
	}
	go d.runObservations()
	go d.run()
	go d.runFailures()
	return d
}

func (d *sourceIncidentNotificationDispatcher) runObservations() {
	for job := range d.observations {
		if job.engine == nil || job.engine.store == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), sourceIncidentNotificationDBTimeout)
		incident, action, err := job.engine.store.CopyTrade().RecordSourceIncidentObservationContext(ctx, job.observation)
		cancel()
		if err != nil {
			logger.Warnf("⚠️ [%s] 异步保存 Binance 数据源事故失败，将由后续观察重试: %v", job.engine.traderID, err)
			continue
		}
		if action != "" {
			d.enqueueNotification(sourceIncidentNotificationJob{
				engine: job.engine, incident: incident, kind: action, health: job.health, now: job.observation.ObservedAt,
			})
		}
	}
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
		ctx, cancel := context.WithTimeout(context.Background(), sourceIncidentNotificationDBTimeout)
		err := job.engine.store.CopyTrade().MarkSourceIncidentMailDeliveryContext(
			ctx, job.incident.ID, job.kind, string(notifier.DeliveryDropped), failure.message, job.now, sourceIncidentMailPolicy(),
		)
		cancel()
		if err != nil {
			logger.Warnf("⚠️ [%s] 恢复 Smart Money 事故邮件重试状态失败: %v", job.engine.traderID, err)
		}
	}
}

func (d *sourceIncidentNotificationDispatcher) enqueueObservation(job sourceIncidentObservationJob) bool {
	select {
	case d.observations <- job:
		return true
	default:
		return false
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

func (d *sourceIncidentNotificationDispatcher) enqueueNotification(job sourceIncidentNotificationJob) {
	if d.enqueue(job) {
		return
	}
	message := fmt.Sprintf("source incident notification queue is full (capacity=%d)", cap(d.jobs))
	if job.engine != nil {
		logger.Warnf("⚠️ [%s] %s", job.engine.traderID, message)
	}
	select {
	case d.failures <- sourceIncidentNotificationFailure{job: job, message: message}:
	default:
		if job.engine != nil {
			logger.Warnf("⚠️ [%s] Smart Money 事故邮件失败队列已满，将由持久化 CLAIMED TTL 重试", job.engine.traderID)
		}
	}
}

var smartMoneyIncidentNotificationDispatcher = newSourceIncidentNotificationDispatcher(sourceIncidentNotificationQueueSize)

func sourceIncidentDurationFromEnv(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func sourceIncidentMailPolicy() store.SourceIncidentMailPolicy {
	return store.SourceIncidentMailPolicy{
		FirstReminderAfter: sourceIncidentDurationFromEnv("COPYTRADE_SOURCE_INCIDENT_FIRST_REMINDER_SECONDS", store.SourceIncidentFirstReminderAfter),
		ReminderAfter:      sourceIncidentDurationFromEnv("COPYTRADE_SOURCE_INCIDENT_REMINDER_SECONDS", store.SourceIncidentReminderAfter),
	}
}

func sourceIncident429Grace() time.Duration {
	return sourceIncidentDurationFromEnv("COPYTRADE_SOURCE_INCIDENT_429_GRACE_SECONDS", 5*time.Minute)
}

func (e *Engine) enqueueSourceIncidentObservation(observation store.SourceIncidentObservation, health *store.CopyTradeSourceHealth) {
	if e == nil || e.store == nil {
		return
	}
	job := sourceIncidentObservationJob{engine: e, observation: observation, health: health}
	if smartMoneyIncidentNotificationDispatcher.enqueueObservation(job) {
		return
	}
	// Notification observations repeat while an incident is active. Dropping a
	// saturated observer job is safer than delaying a close/reduce snapshot or
	// an account API call; the next observation will retry persistence.
	logger.Debugf("📭 [%s] Binance 数据源事故观察队列已满，跳过本次旁路通知观察", e.traderID)
}

func (e *Engine) enqueueSmartMoneySourceIncidentNotification(incident *store.CopyTradeSourceIncident, kind string, health *store.CopyTradeSourceHealth, now time.Time) {
	if incident == nil || health == nil || e.store == nil {
		return
	}
	job := sourceIncidentNotificationJob{engine: e, incident: incident, kind: kind, health: health, now: now}
	// Persist queue failure on a second bounded worker so a busy database can
	// never turn notification overload into source signal latency.
	smartMoneyIncidentNotificationDispatcher.enqueueNotification(job)
}
