// Package notifier 邮件告警通知模块
//
// 设计要点：
//   - 完全独立：不依赖项目其它业务包，只用 Go 标准库 + nofx/logger
//   - 异步发送：通过队列与后台 worker 解耦业务调用与网络 IO
//   - 限流防抖：同 key 告警最小间隔保护，防止异常爆雷邮件刷屏
//   - 兼容容错：未启用或配置缺失时使用 no-op 实现，对调用方零影响
package notifier

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// emailClient SMTP 邮件客户端（无状态，每次 Send 重新建立连接）
type emailClient struct {
	host     string
	port     int
	user     string
	pass     string
	from     string
	fromName string
	timeout  time.Duration
}

// newEmailClient 构造邮件客户端
func newEmailClient(cfg Config) *emailClient {
	from := cfg.From
	if from == "" {
		from = cfg.SMTPUser
	}
	fromName := cfg.FromName
	if fromName == "" {
		fromName = "NOFX Notifier"
	}
	return &emailClient{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		user:     cfg.SMTPUser,
		pass:     cfg.SMTPPass,
		from:     from,
		fromName: fromName,
		timeout:  15 * time.Second,
	}
}

// Send 发送一封邮件
// to: 收件人地址（多个）；subject: UTF-8 主题；body: UTF-8 正文（纯文本）
func (c *emailClient) Send(ctx context.Context, to []string, subject, body string) error {
	if len(to) == 0 {
		return fmt.Errorf("notifier: empty recipient list")
	}
	if c.host == "" || c.user == "" || c.pass == "" {
		return fmt.Errorf("notifier: incomplete smtp config")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	msg := buildMIMEMessage(c.fromName, c.from, to, subject, body)
	auth := smtp.PlainAuth("", c.user, c.pass, c.host)

	// 优先使用隐式 TLS（端口 465 - 163/QQ 等国内邮箱推荐方式）
	if c.port == 465 || c.port == 994 {
		return c.sendViaImplicitTLS(ctx, auth, to, msg)
	}
	// 端口 25/587 → STARTTLS（显式协商）
	return c.sendViaSTARTTLS(ctx, auth, to, msg)
}

// sendViaImplicitTLS 用 SSL/TLS 直连发送（适配 163 端口 465）
func (c *emailClient) sendViaImplicitTLS(ctx context.Context, auth smtp.Auth, to []string, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	dialer := &net.Dialer{Timeout: c.timeout}
	tlsCfg := &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12}

	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("tls dial %s failed: %w", addr, err)
	}
	defer rawConn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = rawConn.SetDeadline(time.Now()) })
	defer stopCancel()
	if err = setSMTPConnectionDeadline(ctx, rawConn, c.timeout); err != nil {
		return err
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("tls handshake %s failed: %w", addr, err)
	}

	client, err := smtp.NewClient(tlsConn, c.host)
	if err != nil {
		return fmt.Errorf("smtp client init failed: %w", err)
	}
	defer client.Close()
	return c.sendSMTPTransaction(client, auth, to, msg)
}

// sendViaSTARTTLS 用 STARTTLS 显式升级发送（适配端口 25/587）
func (c *emailClient) sendViaSTARTTLS(ctx context.Context, auth smtp.Auth, to []string, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	dialer := &net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s failed: %w", addr, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	if err = setSMTPConnectionDeadline(ctx, conn, c.timeout); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, c.host)
	if err != nil {
		return fmt.Errorf("smtp client init failed: %w", err)
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return fmt.Errorf("smtp server %s does not support STARTTLS", addr)
	}
	if err = client.StartTLS(&tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12}); err != nil {
		return fmt.Errorf("smtp STARTTLS failed: %w", err)
	}
	return c.sendSMTPTransaction(client, auth, to, msg)
}

func setSMTPConnectionDeadline(ctx context.Context, conn net.Conn, fallback time.Duration) error {
	deadline := time.Now().Add(fallback)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set smtp deadline failed: %w", err)
	}
	return nil
}

func (c *emailClient) sendSMTPTransaction(client *smtp.Client, auth smtp.Auth, to []string, msg []byte) error {
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp auth failed: %w", err)
	}
	if err := client.Mail(c.from); err != nil {
		return fmt.Errorf("smtp MAIL FROM failed: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT %s failed: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA failed: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write body failed: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("smtp body close failed: %w", err)
	}
	if err = client.Quit(); err != nil {
		return fmt.Errorf("smtp QUIT failed: %w", err)
	}
	return nil
}

// buildMIMEMessage 构造符合 RFC 5322 / MIME 的邮件原文
// 中文主题/正文用 UTF-8 + Base64 编码，避免乱码
func buildMIMEMessage(fromName, from string, to []string, subject, body string) []byte {
	var b strings.Builder

	// 发件人：MIME-encoded 显示名 + 邮箱地址
	b.WriteString("From: ")
	b.WriteString(mimeEncodeWord(fromName))
	b.WriteString(" <")
	b.WriteString(from)
	b.WriteString(">\r\n")

	// 收件人
	b.WriteString("To: ")
	b.WriteString(strings.Join(to, ", "))
	b.WriteString("\r\n")

	// 主题：UTF-8 Base64 编码（防中文乱码）
	b.WriteString("Subject: ")
	b.WriteString(mimeEncodeWord(subject))
	b.WriteString("\r\n")

	// 日期 + Message-ID
	b.WriteString("Date: ")
	b.WriteString(time.Now().Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString(fmt.Sprintf("Message-ID: <%d.%s@nofx.local>\r\n", time.Now().UnixNano(), strings.ReplaceAll(from, "@", ".")))

	// MIME 头
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")

	// 正文用 Base64 + 按 76 字符换行（RFC 2045）
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		b.WriteString("\r\n")
	}

	return []byte(b.String())
}

// mimeEncodeWord 把含中文/非 ASCII 的字段按 RFC 2047 编码
// 形如 =?UTF-8?B?xxx?=，163/QQ/Gmail 均能正确解析
func mimeEncodeWord(s string) string {
	if isASCII(s) {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
