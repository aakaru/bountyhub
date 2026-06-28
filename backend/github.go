package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GitHubIssue struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type GitHubSearchResponse struct {
	TotalCount int           `json:"total_count"`
	Items      []GitHubIssue `json:"items"`
}

// ParseBountyAmount searches title and body for reward patterns and returns amount and currency
func ParseBountyAmount(title, body string) (float64, string) {
	// Look in title first, then body
	searchTexts := []string{title, body}

	// 1. Regular expression patterns for bounty amounts
	// Pattern A: "Bounty: 100 USDC", "Bounty $250", "Bounty: 3 ETH"
	reA := regexp.MustCompile(`(?i)bounty[:\s\-\$]*([\d,\.]+)\s*(usdc|usdt|usd|rtc|eth|btc|sol|matic|dai)`)
	// Pattern B: "$100 Bounty", "10 USDC Bounty"
	reB := regexp.MustCompile(`(?i)(usdc|usdt|usd|rtc|eth|btc|sol|matic|dai)[\s:]*bounty[:\s\-]*([\d,\.]+)`)
	// Pattern C: "$250" or "$ 250" in text (often near word bounty or reward, but let's scan for it generally)
	reC := regexp.MustCompile(`(?i)(?:bounty|reward|prize)[:\s]*\$([\d,\.]+)`)
	reD := regexp.MustCompile(`\$([\d,\.]+)\s*bounty`)

	for _, text := range searchTexts {
		// Try Pattern A
		if matches := reA.FindStringSubmatch(text); len(matches) >= 3 {
			amtStr := strings.ReplaceAll(matches[1], ",", "")
			if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
				return amt, strings.ToUpper(matches[2])
			}
		}

		// Try Pattern B
		if matches := reB.FindStringSubmatch(text); len(matches) >= 3 {
			amtStr := strings.ReplaceAll(matches[2], ",", "")
			if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
				return amt, strings.ToUpper(matches[1])
			}
		}

		// Try Pattern C
		if matches := reC.FindStringSubmatch(text); len(matches) >= 2 {
			amtStr := strings.ReplaceAll(matches[1], ",", "")
			if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
				return amt, "USD"
			}
		}

		// Try Pattern D
		if matches := reD.FindStringSubmatch(text); len(matches) >= 2 {
			amtStr := strings.ReplaceAll(matches[1], ",", "")
			if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
				return amt, "USD"
			}
		}
	}

	// Default fallback: scan for standalone $ values near keywords
	reFallback := regexp.MustCompile(`\$([\d,\.]+)`)
	if matches := reFallback.FindStringSubmatch(title); len(matches) >= 2 {
		amtStr := strings.ReplaceAll(matches[1], ",", "")
		if amt, err := strconv.ParseFloat(amtStr, 64); err == nil {
			return amt, "USD"
		}
	}

	return 0.0, "UNKNOWN"
}

// ClassifyBounty tags the issues based on languages, frameworks, or keywords
func ClassifyBounty(title, body, repoName string, labels []string) string {
	var tags []string
	combined := strings.ToLower(title + " " + body + " " + repoName + " " + strings.Join(labels, " "))

	// Tag Rules
	if strings.Contains(combined, "rust") || strings.Contains(combined, ".rs") {
		tags = append(tags, "Rust")
	}
	if strings.Contains(combined, "golang") || strings.Contains(combined, "go ") || strings.Contains(combined, "go-") {
		tags = append(tags, "Go")
	}
	if strings.Contains(combined, "typescript") || strings.Contains(combined, "ts ") || strings.Contains(combined, "javascript") || strings.Contains(combined, "js ") {
		tags = append(tags, "TypeScript")
	}
	if strings.Contains(combined, "python") || strings.Contains(combined, "py ") {
		tags = append(tags, "Python")
	}
	if strings.Contains(combined, "llm") || strings.Contains(combined, "openai") || strings.Contains(combined, "gpt") || strings.Contains(combined, "agent") || strings.Contains(combined, "ai ") {
		tags = append(tags, "AI")
	}
	if strings.Contains(combined, "solidity") || strings.Contains(combined, "smart contract") || strings.Contains(combined, "ethereum") || strings.Contains(combined, "web3") {
		tags = append(tags, "Web3")
	}
	if strings.Contains(combined, "docker") || strings.Contains(combined, "kubernetes") || strings.Contains(combined, "ci/cd") || strings.Contains(combined, "github actions") {
		tags = append(tags, "DevOps")
	}

	if len(tags) == 0 {
		return "General"
	}
	return strings.Join(tags, ",")
}

// FetchGitHubBounties queries the GitHub search API using the provided token
func FetchGitHubBounties(token string) ([]BountyIssue, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	
	// Prepare search query
	query := "is:open label:bounty,bounties sort:created-desc"
	reqURL := fmt.Sprintf("https://api.github.com/search/issues?q=%s&per_page=50", url.QueryEscape(query))
	
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	
	// Add auth headers if token is present
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "BountyHub-Control-Center")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github api returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var searchResp GitHubSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, err
	}

	var parsedIssues []BountyIssue
	for _, item := range searchResp.Items {
		var labelNames []string
		for _, lbl := range item.Labels {
			labelNames = append(labelNames, lbl.Name)
		}
		
		// Find repo full name. GitHub Search Issues endpoint doesn't return repository details directly sometimes, 
		// but html_url contains: https://github.com/owner/repo/issues/123
		repoFullName := item.Repository.FullName
		if repoFullName == "" {
			parts := strings.Split(item.HTMLURL, "/")
			if len(parts) >= 5 {
				repoFullName = parts[3] + "/" + parts[4]
			}
		}

		amt, cur := ParseBountyAmount(item.Title, item.Body)
		tags := ClassifyBounty(item.Title, item.Body, repoFullName, labelNames)

		parsedIssues = append(parsedIssues, BountyIssue{
			GithubIssueID:      item.ID,
			RepositoryFullName: repoFullName,
			Title:              item.Title,
			Body:               item.Body,
			Labels:             strings.Join(labelNames, ","),
			HTMLURL:            item.HTMLURL,
			CreatedAt:          item.CreatedAt,
			UpdatedAt:          item.UpdatedAt,
			ParsedAmount:       amt,
			Currency:           cur,
			TopicTags:          tags,
		})
	}

	return parsedIssues, nil
}
