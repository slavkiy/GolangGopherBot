package domain

import "time"

type User struct {
	ID                                          int64
	TelegramID                                  int64
	Username, FirstName, LastName, LanguageCode string
	IsBlocked                                   bool
	CreatedAt, UpdatedAt                        time.Time
}

type Project struct {
	ID                                                                                          int64
	UserID                                                                                      int64
	Name, Language, RepoURL, RepoOwner, RepoName, Description, AuthorDescription, Topics, Stars string
	WantsContributors                                                                           bool
	Status                                                                                      string
	PublishedChatID                                                                             int64
	PublishedMessageID                                                                          int
	ChannelChatID                                                                               int64
	ChannelMessageID                                                                            int
	CreatedAt, UpdatedAt                                                                        time.Time
}

const (
	StatusPublished = "published"
	StatusHidden    = "hidden"
	StatusClosed    = "closed"
)
