package slack

import (
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/slack-go/slack"
	"github.com/target/flottbot/models"
	"github.com/target/flottbot/remote"
)

/*
=======================================
Implementation for the Remote interface
=======================================
*/

// Client struct
type Client struct {
	ListenerPort  string
	Token         string
	AppToken      string
	SigningSecret string
}

// validate that Client adheres to remote interface
var _ remote.Remote = (*Client)(nil)

// instantiate a new slack client
func (c *Client) new() *slack.Client {
	api := slack.New(c.Token)
	return api
}

// Reaction implementation to satisfy remote interface
func (c *Client) Reaction(message models.Message, rule models.Rule, bot *models.Bot) {
	if rule.RemoveReaction != "" {
		// Init api client
		api := c.new()
		// Grab a reference to the message
		msgRef := slack.NewRefToMessage(message.ChannelID, message.Timestamp)
		// Remove bot reaction from message
		if err := api.RemoveReaction(rule.RemoveReaction, msgRef); err != nil {
			bot.Log.Error().Msgf("could not add reaction: %v", err)
			return
		}
		bot.Log.Info().Msgf("removed reaction '%s' for rule '%s'", rule.RemoveReaction, rule.Name)
	}
	if rule.Reaction != "" {
		// Init api client
		api := c.new()
		// Grab a reference to the message
		msgRef := slack.NewRefToMessage(message.ChannelID, message.Timestamp)
		// React with desired reaction
		if err := api.AddReaction(rule.Reaction, msgRef); err != nil {
			bot.Log.Error().Msgf("could not add reaction: %v", err)
			return
		}
		bot.Log.Info().Msgf("added reaction '%s' for rule '%s'", rule.Reaction, rule.Name)
	}
}

// Read implementation to satisfy remote interface
// Utilizes the Slack API client to read messages from Slack
func (c *Client) Read(inputMsgs chan<- models.Message, rules map[string]models.Rule, bot *models.Bot) {
	// init api client
	api := c.new()

	// get bot rooms
	bot.Rooms = getRooms(api)

	// get bot id
	rat, err := api.AuthTest()
	if err != nil {
		bot.Log.Error().Msg("the 'slack_token' that was provided was invalid or is unauthorized - closing slack message reader")

		return
	}

	// set the bot ID
	bot.ID = rat.UserID

	if c.AppToken != "" {
		// handle Socket Mode
		// assuming Socket Mode if slack_app_token is provided
		sm := slack.New(
			bot.SlackToken,
			slack.OptionDebug(bot.Debug),
			slack.OptionAppLevelToken(c.AppToken),
			// pass our custom logger through to slack
			slack.OptionLog(log.New(bot.Log, "", 0)),
		)

		// move the above inside readFromSocketMode below :o

		readFromSocketMode(sm, inputMsgs, bot)
	} else if c.SigningSecret != "" {
		// handle Events API setup
		// assuming Events API setup if slack_signing_secret is provided
		readFromEventsAPI(api, c.SigningSecret, inputMsgs, bot)
	}

	// slack is not configured correctly and cli is set to false
	// TODO: move this out of the remote setup
	if c.AppToken == "" && c.SigningSecret == "" && !bot.CLI {
		bot.Log.Error().Msg("cli mode is disabled and tokens are not set up correctly to run the bot")
	}
}

// Send implementation to satisfy remote interface
func (c *Client) Send(message models.Message, bot *models.Bot) {
	bot.Log.Debug().Msgf("sending message '%s'", message.ID)

	api := c.new()

	// check message size and trim if necessary because
	// slack messages have a hard limit of 4000 characters
	if len(message.Output) > slack.MaxMessageTextLength {
		contents := message.Output
		message.Output = contents[:(slack.MaxMessageTextLength-3)] + "..."
	}

	// Timestamp message
	message.EndTime = models.MessageTimestamp()

	// send message  based on type
	switch message.Type {
	case models.MsgTypeDirect, models.MsgTypeChannel, models.MsgTypePrivateChannel:
		send(api, message, bot)
	default:
		bot.Log.Warn().Msg("received unknown message type - no message to send")
	}
}

var interactionsRouter *mux.Router

// InteractiveComponents implementation to satisfy remote interface
// It will serve as a way for your bot to handle advance messaging, such as message attachments.
// When your bot is up and running, it will have an http/https endpoint to handle rules for sending attachments.
func (c *Client) InteractiveComponents(inputMsgs chan<- models.Message, message *models.Message, rule models.Rule, bot *models.Bot) {
	if bot.InteractiveComponents && c.SigningSecret != "" {
		if bot.SlackInteractionsCallbackPath == "" {
			bot.Log.Error().Msg("need to specify a callback path for the 'slack_interactions_callback_path' field in the bot.yml (e.g. \"/slack_events/v1/mybot_dev-v1_interactions\")")
			bot.Log.Warn().Msg("closing interactions reader (will not be able to read interactive components)")
			return
		}
		if interactionsRouter == nil {
			// create router for the Interactive Components server
			interactionsRouter = mux.NewRouter()

			// interaction health check handler
			interactionsRouter.HandleFunc("/interaction_health", getInteractiveComponentHealthHandler(bot)).Methods("GET")

			// Rule handler and endpoint
			ruleHandle := getInteractiveComponentRuleHandler(c.SigningSecret, inputMsgs, message, rule, bot)

			// We use regex for interactions routing for any bot using this framework
			// e.g. /slack_events/v1/mybot_dev-v1_interactions
			if !isValidPath(bot.SlackInteractionsCallbackPath) {
				bot.Log.Error().Msg(`invalid events path - please double check your path value/syntax (e.g. "/slack_events/v1/mybot_dev-v1_interactions")`)
				bot.Log.Warn().Msg("closing interaction components reader (will not be able to read interactive components)")
				return
			}
			interactionsRouter.HandleFunc(bot.SlackInteractionsCallbackPath, ruleHandle).Methods("POST")

			// start Interactive Components server
			go http.ListenAndServe(":4000", interactionsRouter)
			bot.Log.Info().Msgf("slack interactive components server is listening to '%s'", bot.SlackInteractionsCallbackPath)
		}

		// Process the hit rule for Interactive Components, e.g. interactive messages
		processInteractiveComponentRule(rule, message, bot)
	}
}
