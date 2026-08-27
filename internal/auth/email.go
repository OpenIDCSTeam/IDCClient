// Package auth 邮件服务（Resend API）
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// EmailService 邮件服务（使用Resend API）
type EmailService struct {
	APIKey    string
	FromEmail string
}

type resendEmailRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// sendEmail 发送邮件（内部方法）
func (e *EmailService) sendEmail(to, subject, html string) error {
	if e.APIKey == "" || e.FromEmail == "" {
		return fmt.Errorf("邮件服务未配置")
	}

	payload := resendEmailRequest{
		From:    e.FromEmail,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化邮件请求失败: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("POST", "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建HTTP请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送邮件请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("Resend API返回错误状态码: %d", resp.StatusCode)
	}
	return nil
}

// SendVerificationEmail 发送邮箱验证邮件
func (e *EmailService) SendVerificationEmail(toEmail, username, verifyURL string) error {
	html := fmt.Sprintf(`<h2>欢迎注册 OpenIDCS</h2>
<p>您好 %s，</p>
<p>请点击下面的链接验证您的邮箱：</p>
<p><a href="%s">%s</a></p>
<p>如果您没有注册账号，请忽略此邮件。此链接24小时内有效。</p>`, username, verifyURL, verifyURL)
	return e.sendEmail(toEmail, "OpenIDCS - 邮箱验证", html)
}

// SendPasswordResetEmail 发送密码重置邮件
func (e *EmailService) SendPasswordResetEmail(toEmail, username, resetLink string) error {
	html := fmt.Sprintf(`<h2>密码重置请求</h2>
<p>您好 %s，</p>
<p>请点击下面的链接重置您的密码：</p>
<p><a href="%s">%s</a></p>
<p>如果您没有请求重置密码，请忽略此邮件。此链接24小时内有效。</p>`, username, resetLink, resetLink)
	return e.sendEmail(toEmail, "OpenIDCS - 密码重置", html)
}

// SendEmailChangeVerification 发送邮箱变更验证邮件
func (e *EmailService) SendEmailChangeVerification(toEmail, username, verifyURL string) error {
	html := fmt.Sprintf(`<h2>邮箱变更验证</h2>
<p>您好 %s，</p>
<p>请点击下面的链接验证您的新邮箱地址：</p>
<p><a href="%s">%s</a></p>
<p>如果您没有请求变更邮箱，请忽略此邮件。此链接24小时内有效。</p>`, username, verifyURL, verifyURL)
	return e.sendEmail(toEmail, "OpenIDCS - 邮箱变更验证", html)
}

// SendTestEmail 发送测试邮件
func (e *EmailService) SendTestEmail(toEmail, subject, body string) error {
	if subject == "" {
		subject = "OpenIDCS - 测试邮件"
	}
	html := body
	if html == "" {
		html = fmt.Sprintf(`<h2>测试邮件</h2>
<p>这是一封来自 OpenIDCS 系统的测试邮件。</p>
<p>测试时间: %s</p>`, time.Now().Format("2006-01-02 15:04:05"))
	}
	return e.sendEmail(toEmail, subject, html)
}
