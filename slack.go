package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nlopes/slack"
	"github.com/pkg/errors"
)

const (
	PresenceAway   = "away"
	PresenceActive = "auto"
)

type Presence string

type Action struct {
	Presence    Presence
	StatusText  string
	StatusEmoji string
	DnD         bool
	Duration    int64
}

type ActionTemplate struct {
	Name   string
	TeamId string
	Action
}

func (t ActionTemplate) ToString() string {
	statusText := ""
	if t.StatusText != "" {
		statusText = fmt.Sprintf(" %s", t.StatusText)
	}

	emojiText := ""
	if t.StatusEmoji != "" {
		emojiText = fmt.Sprintf(" (%s)", t.StatusEmoji)
	}

	dndText := ""
	if t.DnD {
		dndText = fmt.Sprintf(" DND")
	}

	durationText := ""
	if t.Duration != 0 {
		durationText = fmt.Sprintf(" for %s", time.Duration(t.Duration).String())
	}

	return fmt.Sprintf("%s = %s%s%s%s", t.Name, statusText, emojiText, dndText, durationText)
}

type ClearStatusRequest struct {
	SlackPayload
	Global bool
}

type ListTriggersRequest struct {
	SlackPayload
	Global bool
}

type TriggerRequest struct {
	SlackPayload
	Name string
}

type CreateTriggerRequest struct {
	SlackPayload
	Definition string
}

type SlackPayload struct {
	UserId   string
	UserName string
	TeamId   string
	TeamName string
}

func ClearStatus(r ClearStatusRequest) error {
	fmt.Printf("Clearing Status for %s(%s) on %s(%s)\n",
		r.UserName, r.UserId, r.TeamName, r.TeamId)

	action := Action{
		Presence: PresenceActive,
	}
	return updateSlackStatus(r.SlackPayload, action)
}

func ListTriggers(r ListTriggersRequest) (slack.Msg, error) {
	fmt.Printf("Listing Triggers for %s(%s) on %s(%s)\n",
		r.UserName, r.UserId, r.TeamName, r.TeamId)

	client, err := NewStorageClient()
	if err != nil {
		return slack.Msg{}, err
	}

	userDir := r.UserId + "/"
	blobNames, err := client.listContainer("triggers", userDir)
	if err != nil {
		return slack.Msg{}, err
	}

	triggers := make([]string, len(blobNames))
	for i, blobName := range blobNames {
		triggerName := strings.TrimPrefix(blobName, userDir)
		trigger, err := getTrigger(r.UserId, triggerName)
		if err != nil {
			return slack.Msg{}, err
		}

		triggers[i] = trigger.ToString()
	}

	msg := slack.Msg{
		ResponseType: slack.ResponseTypeEphemeral,
		Text:         strings.Join(triggers, "\n"),
	}

	return msg, nil
}

func Trigger(r TriggerRequest) error {
	fmt.Printf("Triggering %s from %s(%s) on %s(%s)\n",
		r.Name, r.UserName, r.UserId, r.TeamName, r.TeamId)

	action, err := getTrigger(r.UserId, r.Name)
	if err != nil {
		return err
	}

	return updateSlackStatus(r.SlackPayload, action.Action)
}

func updateSlackStatus(payload SlackPayload, action Action) error {
	token, err := getSlackToken()
	if err != nil {
		return err
	}

	api := slack.New(token, slack.OptionDebug(*debugFlag))

	err = api.SetUserPresence(string(action.Presence))
	if err != nil {
		err = errors.Wrap(err, "could not set presence")
		return err
	}

	err = api.SetUserCustomStatus(action.StatusText, action.StatusEmoji, action.Duration)
	if err != nil {
		return errors.Wrap(err, "could not set status")
	}

	if !action.DnD {
		dndState, err := api.GetDNDInfo(&payload.UserId)
		if err != nil {
			return errors.Wrapf(err, "could not retrieve user's current DND state")
		}
		if dndState.SnoozeEnabled {
			_, err = api.EndSnooze()
			if err != nil {
				return errors.Wrap(err, "could not end do not disturb")
			}
		}
	} else if action.DnD {
		_, err = api.SetSnooze(int(action.Duration))
		if err != nil {
			return errors.Wrap(err, "could not set do not disturb")
		}
	}
	return nil
}

// CreateTrigger accepts a trigger definition and saves it
func CreateTrigger(r CreateTriggerRequest) error {
	fmt.Printf("CreateTrigger %s from %s(%s) on %s(%s)\n",
		r.Definition, r.UserName, r.UserId, r.TeamName, r.TeamId)

	tmpl, err := parseTemplate(r.Definition)
	if err != nil {
		return err
	}

	tmpl.TeamId = r.TeamId

	tmplB, err := json.Marshal(tmpl)
	if err != nil {
		return errors.Wrapf(err, "error marshaling trigger %s for %s(%s) on %s(%s): %#v",
			tmpl.Name, r.UserName, r.UserId, r.TeamName, r.TeamId, tmpl)
	}

	client, err := NewStorageClient()
	if err != nil {
		return err
	}

	key := path.Join(r.UserId, tmpl.Name)
	return client.setBlob("triggers", key, tmplB)
}

func getTrigger(userId string, name string) (ActionTemplate, error) {
	client, err := NewStorageClient()
	if err != nil {
		return ActionTemplate{}, err
	}

	key := path.Join(userId, name)
	b, err := client.getBlob("triggers", key)
	if err != nil {
		if strings.Contains(err.Error(), "BlobNotFound") {
			return ActionTemplate{}, errors.Errorf("trigger %s not registered", name)
		}
		return ActionTemplate{}, err
	}

	var action ActionTemplate
	err = json.Unmarshal(b, &action)
	if err != nil {
		return ActionTemplate{}, errors.Wrapf(err, "error unmarshaling trigger %s: %s", name, string(b))
	}

	return action, nil
}

// parseAction definition into an Action
// Example:
// vacation = vacay (🌴) DND for 1w
// name = vacation
// status = vacay
// emoji = 🌴
// DND = Yes
// duration = 1w
func parseTemplate(def string) (ActionTemplate, error) {
	// Test out at https://regex101.com/r/8v180Z/5
	const pattern = `^([\w-_]+)[ ]?=(?:[ ]?(.+))?[ ]+\((.*)\)( DND)?(?: for (\d[wdhms]+))?$`
	r := regexp.MustCompile(pattern)
	match := r.FindStringSubmatch(def)
	if len(match) == 0 {
		return ActionTemplate{}, errors.Errorf("Invalid trigger definition %q. Try /create-trigger vacation = I'm on a boat! (⛵️) DND for 1w", def)
	}

	duration, err := parseDuration(match[5])
	if err != nil {
		return ActionTemplate{}, errors.Errorf("invalid duration in trigger definition %q, here are some examples: 15m, 1h, 2d, 1w", match[5])
	}

	template := ActionTemplate{
		Name: match[1],
		Action: Action{
			Presence:    PresenceAway,
			StatusText:  match[2],
			StatusEmoji: match[3],
			DnD:         match[4] != "",
			Duration:    int64(duration.Minutes()),
		},
	}
	return template, nil
}

const (
	day  = 24 * time.Hour
	week = 7 * day
)

func parseDuration(value string) (time.Duration, error) {
	if value == "" {
		return time.Duration(0), nil
	}

	r := regexp.MustCompile(`^(\d+)([wd])$`)
	match := r.FindStringSubmatch(value)
	if len(match) == 0 {
		return time.ParseDuration(value)
	}

	num, err := strconv.Atoi(match[1])
	if err != nil {
		return time.Duration(0), err
	}

	var unit time.Duration
	switch match[2] {
	case "d":
		unit = day
	case "w":
		unit = week
	}
	return time.Duration(num) * unit, nil
}

func getSlackToken() (string, error) {
	client, err := getKeyVaultClient()
	if err != nil {
		fmt.Println("Loading slack token from env var...")
		token := os.Getenv("SLACK_TOKEN")
		if token == "" {
			return "", fmt.Errorf("could not authenticate using ambient environment: %s", err.Error())
		}
		return token, nil
	}

	fmt.Println("Loading slack token from vault...")
	grr, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, err := client.GetSecret(grr, vaultURL, "slack-token", "")
	if err != nil {
		defer cancel()
		return "", fmt.Errorf("could not load slack token from vault: %s", err)
	}

	return *result.Value, nil
}