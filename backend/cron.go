package main

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// StartCronScheduler runs a background goroutine syncing bounties at the scheduled time daily
func StartCronScheduler() {
	// Sync immediately on startup in a separate goroutine
	go func() {
		log.Println("Running startup bounty sync...")
		if err := SyncAndSendDigest(); err != nil {
			log.Printf("Startup sync error: %v", err)
		}
	}()

	// Check settings every 1 minute to trigger digest email on time
	ticker := time.NewTicker(1 * time.Minute)
	var lastSentDay string
	go func() {
		for range ticker.C {
			var settings UserSetting
			if err := DB.First(&settings).Error; err != nil {
				continue
			}

			digestTime := settings.DigestTime
			if digestTime == "" {
				digestTime = "09:00"
			}

			now := time.Now()
			currentTimeStr := now.Format("15:04")
			currentDayStr := now.Format("2006-01-02")

			if currentTimeStr == digestTime && lastSentDay != currentDayStr {
				log.Printf("Scheduled digest time reached (%s). Starting sync and digest...", digestTime)
				lastSentDay = currentDayStr
				if err := SyncAndSendDigest(); err != nil {
					log.Printf("Scheduled digest sync error: %v", err)
				}
			}
		}
	}()
}

// SyncAndSendDigest fetches latest bounties from GitHub, caches them in SQLite, and emails new ones
func SyncAndSendDigest() error {
	var settings UserSetting
	if err := DB.First(&settings).Error; err != nil {
		return fmt.Errorf("failed to fetch user settings: %w", err)
	}

	// Fetch bounties from GitHub using user stored token (or system env GITHUB_PAT)
	token := settings.GithubToken
	if token == "" {
		// Fallback to system env variable
		// We'll read from main config or environment later
	}

	issues, err := FetchGitHubBounties(token)
	if err != nil {
		return fmt.Errorf("failed to fetch from github: %w", err)
	}

	// Insert/Update issues, and track which ones are newly created/inserted
	var newBounties []BountyIssue
	for _, iss := range issues {
		var existing BountyIssue
		res := DB.Where("github_issue_id = ?", iss.GithubIssueID).First(&existing)
		if res.Error != nil {
			// This is a new bounty we haven't seen in the DB
			if err := DB.Create(&iss).Error; err == nil {
				// Apply filters to decide if it goes in email
				if IsBountyMatch(iss, settings) {
					newBounties = append(newBounties, iss)
				}
			}
		} else {
			// Update existing bounty cache details
			DB.Model(&existing).Updates(BountyIssue{
				Title:        iss.Title,
				Body:         iss.Body,
				UpdatedAt:    iss.UpdatedAt,
				ParsedAmount: iss.ParsedAmount,
				Currency:     iss.Currency,
				TopicTags:    iss.TopicTags,
			})
		}
	}

	log.Printf("Sync completed. Cached %d issues, found %d new ones matching user filters.", len(issues), len(newBounties))

	// Send email digest if we have new matching issues and SMTP is configured
	if len(newBounties) > 0 && settings.Email != "" && (settings.SMTPHost != "" || os.Getenv("SMTP_HOST") != "") {
		err := SendEmailDigest(settings, newBounties)
		if err != nil {
			log.Printf("SMTP email send failure: %v", err)
			return err
		}
	}

	return nil
}

// IsBountyMatch filters bounties based on user settings
func IsBountyMatch(bounty BountyIssue, settings UserSetting) bool {
	// Min amount filter
	if bounty.ParsedAmount > 0 && bounty.ParsedAmount < settings.MinBountyAmount {
		return false
	}

	// Language tags filter
	if settings.FilterLanguages != "" {
		languages := strings.Split(strings.ToLower(settings.FilterLanguages), ",")
		bountyTags := strings.ToLower(bounty.TopicTags)
		
		match := false
		for _, lang := range languages {
			lang = strings.TrimSpace(lang)
			if lang == "" {
				continue
			}
			if strings.Contains(bountyTags, lang) {
				match = true
				break
			}
		}
		if !match && bounty.TopicTags != "General" {
			return false
		}
	}

	return true
}

// SendEmailDigest constructs and sends HTML email
func SendEmailDigest(settings UserSetting, bounties []BountyIssue) error {
	// Build HTML Body
	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("<html><body style=\"font-family: sans-serif; background-color: #0d0e15; color: #f5f5f7; padding: 20px;\">")
	bodyBuilder.WriteString("<h1 style=\"color: #4d65ff; border-bottom: 2px solid #1a1c29; padding-bottom: 10px;\">Daily Bounty Digest</h1>")
	bodyBuilder.WriteString(fmt.Sprintf("<p style=\"color: #86868b;\">We found <strong>%d</strong> new bounties matching your stack preferences today.</p><br/>", len(bounties)))

	for _, b := range bounties {
		bodyBuilder.WriteString("<div style=\"background-color: #12131e; border: 1px solid #1f2136; border-radius: 8px; padding: 15px; margin-bottom: 15px;\">")
		bodyBuilder.WriteString("<table width=\"100%\" style=\"border-collapse: collapse;\"><tr>")
		bodyBuilder.WriteString(fmt.Sprintf("<td><h3 style=\"margin: 0; color: #ffffff;\">%s</h3></td>", b.Title))
		
		// Highlight reward
		rewardText := "Unparsed"
		if b.ParsedAmount > 0 {
			rewardText = fmt.Sprintf("%.2f %s", b.ParsedAmount, b.Currency)
		}
		bodyBuilder.WriteString(fmt.Sprintf("<td align=\"right\"><span style=\"background-color: rgba(77, 101, 255, 0.2); border: 1px solid #4d65ff; color: #ffffff; padding: 4px 8px; border-radius: 4px; font-weight: bold;\">%s</span></td>", rewardText))
		
		bodyBuilder.WriteString("</tr></table>")
		bodyBuilder.WriteString(fmt.Sprintf("<p style=\"margin: 8px 0; color: #86868b; font-size: 0.85em;\">Repository: <strong>%s</strong></p>", b.RepositoryFullName))
		
		// Display tags
		tags := strings.Split(b.TopicTags, ",")
		bodyBuilder.WriteString("<p style=\"margin: 8px 0;\">")
		for _, tag := range tags {
			bodyBuilder.WriteString(fmt.Sprintf("<span style=\"background-color: #1b1c2a; color: #86868b; font-size: 0.8em; padding: 2px 6px; margin-right: 5px; border-radius: 3px;\">%s</span>", tag))
		}
		bodyBuilder.WriteString("</p>")

		bodyBuilder.WriteString(fmt.Sprintf("<p style=\"margin-top: 15px;\"><a href=\"%s\" style=\"color: #4d65ff; text-decoration: none; font-weight: bold;\">View on GitHub →</a></p>", b.HTMLURL))
		bodyBuilder.WriteString("</div>")
	}

	bodyBuilder.WriteString("<hr style=\"border: none; border-top: 1px solid #1a1c29; margin: 30px 0;\"/>")
	bodyBuilder.WriteString("<p style=\"color: #4e4e52; font-size: 0.75em;\">This email was sent automatically by your Bounty Control Center. You can update your settings at any time on localhost.</p>")
	bodyBuilder.WriteString("</body></html>")

	htmlBody := bodyBuilder.String()
	subject := fmt.Sprintf("BountyHub: %d New Bounties Discovered", len(bounties))
	
	// Read SMTP credentials from environment with settings fallback
	smtpHost := os.Getenv("SMTP_HOST")
	if smtpHost == "" {
		smtpHost = settings.SMTPHost
	}

	smtpPort := settings.SMTPPort
	if envPort := os.Getenv("SMTP_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			smtpPort = p
		}
	}

	smtpUser := os.Getenv("SMTP_USER")
	if smtpUser == "" {
		smtpUser = settings.SMTPUser
	}

	smtpPass := os.Getenv("SMTP_PASS")
	if smtpPass == "" {
		smtpPass = settings.SMTPPass
	}

	// SMTP Auth
	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	to := []string{settings.Email}
	msg := []byte("To: " + settings.Email + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-version: 1.0;\nContent-Type: text/html; charset=\"UTF-8\";\r\n\r\n" +
		htmlBody)
	
	addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
	log.Printf("Connecting to SMTP server at %s to send digest...", addr)
	
	err := smtp.SendMail(addr, auth, smtpUser, to, msg)
	if err != nil {
		return err
	}
	
	log.Println("Email digest successfully sent!")
	return nil
}
