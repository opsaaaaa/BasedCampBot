package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-co-op/gocron/v2"
	"github.com/mmcdole/gofeed"
	"github.com/pelletier/go-toml/v2"
)

const VERSION = "0.0.3"

type Config struct {
    RemoveCommands bool
    Feed struct {
        Url string
        Type string
        CronSchedule string
        PostInterval int
        NoCache bool
    }
    DiscordBot struct {
        Username string
        ClientID string
        ClientSecret string
        Token string
        Status string
        Retries int
        Logging uint8
    }
    DiscordMsg struct {
        ArchiveDuration int
        NotifyPrefix string
        TimeFormat string
    }
    DiscordServer struct {
        GuildID string
        PostChannelID string
        NotifyChannelID string
    }
    Discord struct {
        MaxTitleLength int
        MaxMessageLength int
    }
}

const VisitedSeen = uint8(0)
const VisitedInit = uint8(1)
const VisitedPosted = uint8(2)

const LoggingNone = uint8(0)
const LogProd = uint8(1)
const LogDebug = uint8(2)

var (
    dg *discordgo.Session
    config *Config
    schdl gocron.Scheduler
    visitedList map[string]uint8
    lastPublished time.Time
)

var (
    integerOptionMinValue          = 1.0
    // dmPermission                   = false
    // defaultMemberPermissions int64 = discordgo.PermissionManageServer

    commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate) error {
        "ping": cmdPingpong,
        "checkfeed": cmdCheckfeed,
        "checkconfig": cmdCheckConfig,
        "postlatest": cmdPostlatest,
        "postnew": cmdPostNewFeed,
        "status": cmdStatus,
    }
    commands = []*discordgo.ApplicationCommand{
        {
            Name: "ping",
            Description: "pong",
        },
        {
            Name: "status",
            Description: "Check when the bot will start checking for the next episode.",
        },
        {
            Name: "checkfeed",
            Description: "Manually check the feed for a new post.",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionInteger,
                    Name:        "count",
                    Description: "Number of results to display.",
                    MinValue:    &integerOptionMinValue,
                    MaxValue:    15,
                    Required:    false,
                },
                {
                    Type:        discordgo.ApplicationCommandOptionBoolean,
                    Name:        "header",
                    Description: "Display the header?",
                    Required:    false,
                },
            },
        },
        {
            Name: "checkconfig",
            Description: "Check channels, feed source and other config settings.",
        },
        {
            Name: "postlatest",
            Description: "repost the latest item in the feed.",
        },
        {
            Name: "postnew",
            Description: "repost the latest item in the feed.",
        },
    }
)

func init() {
    config = GetConfig()
    InitDiscord()
    InitScheduler()
}

func logLvlF(level uint8, format string, v ...any) {
    if config.DiscordBot.Logging >= level {
        log.Printf(format, v...)
    }
}

func logLvlLn(level uint8, v ...any) {
    if config.DiscordBot.Logging >= level {
        log.Println(v...)
    }
}

func connectToDiscordWithRetry() error {
    retryDelay := 5 * time.Second // Base delay between retries
    for attempt := 0; attempt <= config.DiscordBot.Retries; attempt++ {
        err := dg.Open()
        if err == nil {
            return nil
        }

        logLvlF(LogDebug, "Connection attempt %d/%d failed: %v", attempt+1, config.DiscordBot.Retries, err)

        if attempt < config.DiscordBot.Retries {
            // Exponential backoff: 5s, 10s, 20s, etc.
            sleepTime := retryDelay * time.Duration(1<<uint(attempt))
            logLvlF(LogDebug, "Waiting %v before next attempt...", sleepTime)
            time.Sleep(sleepTime)
        }
    }
    return fmt.Errorf("failed to connect to Discord after %d retries", config.DiscordBot.Retries)
}

func main() {
    err := connectToDiscordWithRetry()
    if err != nil {
        logLvlLn(LogProd, "Error opening connection after retries:", err)
        os.Exit(1)
        return
    }
    defer dg.Close()
    logLvlLn(LogDebug, "Discord Connected!")
    registeredCommands := UpDiscord()

    InitFeed()

    schdl.Start()
    logLvlLn(LogDebug, "Cron Scheduler Started.")
    defer schdl.Shutdown()

    logLvlLn(LogProd, "Bot is now running. Press CTRL-C to exit.")
    sc := make(chan os.Signal, 1)
    signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
    <-sc

    DownDiscord(registeredCommands)
    os.Exit(0)
}

func onReady(s *discordgo.Session, event *discordgo.Ready) {
    logLvlF(LogProd, "Logged in as %s", event.User.String())
    _ = s.UpdateGameStatus(0, config.DiscordBot.Status)
}

func onCronCallback() {
    logLvlLn(LogDebug, "Cron Callback")

    if !time.Now().After(lastPublished.Add(
        time.Duration(config.Feed.PostInterval) * time.Hour,
    )) {
        logLvlLn(LogDebug, "Skipped check outside of post interval")
        return
    }

    feed, err := GetFeed()
    if err != nil {
        logLvlLn(LogProd, "Error getting feed.", err)
        return
    }

    for _, item := range feed.Items {
        if val, found := visitedList[item.GUID]; !found || val == VisitedSeen {
            _ = PostFeedItem(feed, item)
        }
    }

    UpdateVisitedList(feed, VisitedSeen)
}



func UpdateVisitedList(feed *gofeed.Feed, visitType uint8) {
    guids := make(map[string]bool,len(feed.Items))
    for _, item := range feed.Items {
        guids[item.GUID] = true
        if _, found := visitedList[item.GUID]; !found {
            visitedList[item.GUID] = visitType
        }
    }
    for key := range visitedList {
        if _, found := guids[key]; !found {
            delete(visitedList, key)
        }
    }
}

func QueryAllFeedItems() (*gofeed.Feed, error) {
    feed, err := GetFeed()
    if err != nil {
        logLvlLn(LogProd, "Error getting feed.", err)
        return feed, err
    }
    return feed, nil
}

func QueryNewFeedItems() (*gofeed.Feed, []*gofeed.Item, error) {
    result := make([]*gofeed.Item, 0)
    feed, err := GetFeed()
    if err != nil {
        logLvlLn(LogProd, "Error getting feed.", err)
        return feed, result, err
    }

    for _, item := range feed.Items {
        if val, found := visitedList[item.GUID]; !found || val == VisitedSeen {
            result = append(result, item)
        }
    }
    return feed, result, nil
}

func PostFeedItem(feed *gofeed.Feed, item *gofeed.Item) error {
    logLvlLn(LogDebug, "Posting.",item.GUID, item.Title)

    var body string = ""
    for _, link := range item.Links {
        body = body + link + "\n"
    }
    body = body + item.Description + "\n"
    body = truncateString(body, config.Discord.MaxMessageLength)

    title := truncateString(
        item.PublishedParsed.Format(config.DiscordMsg.TimeFormat)+" - "+item.Title,
        config.Discord.MaxTitleLength,
    )

    postMsg, err := dg.ForumThreadStart(config.DiscordServer.PostChannelID,title,config.DiscordMsg.ArchiveDuration,body)
    if err != nil {
        logLvlLn(LogProd, "Error making ForumThread post.", err, title)
        return err
    }
    logLvlF(LogDebug, "Created ForumThread post. '%s' [%s] (%s)", title, postMsg.ID, item.GUID)

    lastPublished = *item.PublishedParsed
    visitedList[item.GUID] = VisitedPosted

    body = truncateString(
        config.DiscordMsg.NotifyPrefix + " " +
        "https://discord.com/channels/"+config.DiscordServer.GuildID+"/"+postMsg.ParentID+"/"+postMsg.ID+"\n"+
        item.Title,
        config.Discord.MaxMessageLength,
    )

    notifyMsg, err := dg.ChannelMessageSend(config.DiscordServer.NotifyChannelID, body)

    if err != nil {
        logLvlLn(LogProd, "Error sending discord notify message.",err, body)
        return err
    }
    logLvlLn(LogDebug, "Created notification message", notifyMsg.ID, body)
    logLvlLn(LogProd, "Posted new episode.")
    return nil
}


func InitFeed() {
    feed, err := GetFeed()
    visitedList = make(map[string]uint8, len(feed.Items))
    if err != nil {
        log.Fatalln("Error getting feed.", err)
    }
    for _, item := range feed.Items {
        if item.PublishedParsed.After(lastPublished) {
            lastPublished = *item.PublishedParsed
        }
    }
    UpdateVisitedList(feed, VisitedInit)
    logLvlLn(LogDebug, "Added visitedList.", visitedList)
}

func InitScheduler() {
    s, err := gocron.NewScheduler()
    schdl = s
    if err != nil {
        log.Fatalln("Error initializing Scheduler", err)
    }
    _, err = schdl.NewJob(gocron.CronJob(config.Feed.CronSchedule, true), gocron.NewTask(onCronCallback))
    if err != nil {
        log.Fatalln("Error Adding Scheduler Job", err)
    }
}

func InitDiscord() {
    var err error
    dg, err = discordgo.New("Bot " + config.DiscordBot.Token)
    if err != nil {
        log.Fatalln("Error creating discordgo session.", err)
    }

    dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
        if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
            logCmd(i.ApplicationCommandData().Name, i)
            err = h(s, i)
            if err != nil {
                logLvlLn(LogProd, "Error "+i.ApplicationCommandData().Name, err)
            }
        }
    })
    dg.AddHandler(onReady)
    dg.Identify.Intents = discordgo.IntentsGuildMessages
}

func UpDiscord() []*discordgo.ApplicationCommand {
    registeredCommands := make([]*discordgo.ApplicationCommand, len(commands))
    for i, v := range commands {
        cmd, err := dg.ApplicationCommandCreate(dg.State.User.ID, config.DiscordServer.GuildID, v)
        if err != nil {
            log.Panicf("Cannot create '%v' command: %v", v.Name, err)
        }
        registeredCommands[i] = cmd
    }
    logLvlLn(LogProd, "Registered Discord Commands")
    return registeredCommands
}

func DownDiscord(registeredCommands []*discordgo.ApplicationCommand) {
    if config.RemoveCommands {
        logLvlLn(LogProd, "Removing commands...")
        // // We need to fetch the commands, since deleting requires the command ID.
        // // We are doing this from the returned commands on line 375, because using
        // // this will delete all the commands, which might not be desirable, so we
        // // are deleting only the commands that we added.
        // registeredCommands, err := s.ApplicationCommands(s.State.User.ID, *GuildID)
        // if err != nil {
        //     log.Fatalf("Could not fetch registered commands: %v", err)
        // }

        for _, v := range registeredCommands {
            err := dg.ApplicationCommandDelete(dg.State.User.ID, config.DiscordServer.GuildID, v.ID)
            if err != nil {
                log.Panicf("Cannot delete '%v' command: %v", v.Name, err)
            }
        }
    }
}

func cmdPostlatest(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var content string
    feed, err := QueryAllFeedItems()
    if err != nil {
        content = "Can't query feed '"+config.Feed.Url+"'."
    } else if len(feed.Items) > 0 {
        item := feed.Items[0]
        err = PostFeedItem(feed, item)
        if err != nil {
            content = content + "Failed '"+ item.Title + "'.\n"
        } else {
            content = content + "Posted '"+ item.Title + "'.\n"
        }
    } else {
        content = "No new items in feed to post."
    }
    UpdateVisitedList(feed, VisitedSeen)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdCheckConfig(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var content string
    content += "```\n"
    content += fmt.Sprintf("Post to https://discord.com/channels/%s/%s\n", config.DiscordServer.GuildID, config.DiscordServer.PostChannelID)
    content += fmt.Sprintf("Notify to https://discord.com/channels/%s/%s\n", config.DiscordServer.GuildID, config.DiscordServer.NotifyChannelID)
    content += fmt.Sprintf("Feed Source `%s`\n", config.Feed.Url)
    content += fmt.Sprintf("Notify Prefix `%s`\n", config.DiscordMsg.NotifyPrefix)
    content += fmt.Sprintf("TimeFormat `%s`\n", config.DiscordMsg.TimeFormat)
    content += fmt.Sprintf("Post Interval every `%d` hours", config.Feed.PostInterval)
    content +=
        "\nCron Job Schedule\n"+
        config.Feed.CronSchedule+"\n"+
        "* * * * * *\n"+
        "| | | | | +----- day of the week (0 - 7) (Sunday is 0 and 7)\n"+
        "| | | | +------- month (1 - 12)\n"+
        "| | | +--------- day of the month (1 - 31)\n"+
        "| | +----------- hour (0 - 23)\n"+
        "| +------------- minute (0 - 59)\n"+
        "+--------------- second (0 - 59)\n"+
        "```\n"
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdCheckfeed(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var maxCount = 3
    var showHeader = false
    for _, opt := range i.ApplicationCommandData().Options {
        switch opt.Name {
            case "count":
                maxCount = int(opt.IntValue())
            case "header":
                showHeader = bool(opt.BoolValue())
            default:
        }
    }
    var content string
    body, header, err := RequestFeed()
    var feed *gofeed.Feed
    if err == nil {
        feed, err = ParseFeed(string(body))
    }
    if err != nil {
        content = "Can't query feed '"+config.Feed.Url+"'."
    } else if len(feed.Items) > 0 {
        if showHeader {
            content += "```\n"
            keys := make([]string, 0, len(header))
            for k := range header {
                keys = append(keys, k)
            }
            slices.Sort(keys)
            for _, key := range keys {
                content += fmt.Sprintf("%v: %v\n", key, header[key])
            }
            content += "```\n"
        }
        for x, item := range feed.Items {
            if x >= maxCount {
                break
            }
            checkbox := "✅"
            val, found := visitedList[item.GUID] 
            if !found {
                checkbox = "⭕"
            } else if val == VisitedInit {
                checkbox = "🔷"
            } else if val == VisitedSeen {
                checkbox = "🔴"
            }
            content += fmt.Sprintf(
                "%d. %s - %s - **%s**. *(%s)*\n",
                x,
                checkbox,
                item.PublishedParsed.Format(config.DiscordMsg.TimeFormat),
                item.Title,
                item.GUID,
            )
        }
        if len(feed.Items) > maxCount {
            content += fmt.Sprintf("*%d more...*", len(feed.Items) - maxCount)
        }
    } else {
        content = "No items in feed."
    }
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdPostNewFeed(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var content string
    feed, items, err := QueryNewFeedItems()
    if err != nil {
        content = "Can't query feed '"+config.Feed.Url+"'."
    } else if len(items) > 0 {
        for _, item := range items {
            err = PostFeedItem(feed, item)
            if err != nil {
                content = content + "Failed '"+ item.Title + "'.\n"
            } else {
                content = content + "Posted '"+ item.Title + "'.\n"
            }
        }
    } else {
        content = "No new items in feed to post."
    }
    UpdateVisitedList(feed, VisitedSeen)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdPingpong(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    content := fmt.Sprintf("Pong! `Version %s`", VERSION)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdStatus(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    sleepuntil := lastPublished.Add(
        time.Duration(config.Feed.PostInterval) * time.Hour,
    )
    now := time.Now()
    content := ""
    if now.After(sleepuntil) {
        content += fmt.Sprintf(
            "⏳ Waiting for new posts since `%s`, `%.2f` hours ago.\n", 
            sleepuntil.Format(time.RFC822Z),
            now.Sub(sleepuntil).Hours(),
        )
    } else {
        content += fmt.Sprintf(
            "⏰ Sleeping until `%s` in `%.2f` hours.\n",
            sleepuntil.Format(time.RFC822Z),
            sleepuntil.Sub(now).Hours(),
        )
    }
    content += fmt.Sprintf(
        "🗓️ Last Published on `%s`, `%.2f` hours ago.\n",
        lastPublished.Format(time.RFC822Z),
        now.Sub(lastPublished).Hours(),
    )
    var nextRun time.Time
    var lastRun time.Time
    for _, job := range schdl.Jobs() {
        nextRun, _ = job.NextRun()
        lastRun, _ = job.LastRun()
        // if err == nil && (nextRun.IsZero() || nextRun.After(jobNextRun)) {
        //     nextRun = jobNextRun
        // }
        // jobLastRun, err := job.LastRun()
        // if err == nil && (lastRun.IsZero() || lastRun.Before(jobLastRun)) {
        //     lastRun = jobLastRun
        // }
    }
    content += fmt.Sprintf(
        "⏮️ Previous check ran at `%s`.\n",
        lastRun.Format(time.RFC822Z),
    )
    content += fmt.Sprintf(
        "⏭️ Next check scheduled for `%s`.\n",
        nextRun.Format(time.RFC822Z),
    )

    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func logCmd(cmd string,i *discordgo.InteractionCreate) {
    var username string = ""
    if i.Member != nil {
        username = i.Member.User.Username
    } else if i.User != nil {
        username = i.User.Username
    }
    logLvlF(LogProd, "%s ran ./%s", username, cmd)
}

func GetFeed() (feed *gofeed.Feed, err error) {
    body, _, err := RequestFeed()
    if err != nil {
        return feed, err
    }
    return ParseFeed(string(body))
}

func RequestFeed() (body []byte, header http.Header, err error) {
    client := &http.Client{}
    req, err := http.NewRequest("GET", config.Feed.Url, nil)
    if err != nil {
        return body, header, err
    }
    if config.Feed.NoCache {
        req.Header.Set("Pragma", "no-cache")
        req.Header.Set("Cache-Control", "no-cache")
    }
    resp, err := client.Do(req)
    if err != nil {
        return body, header, err
    }
    defer resp.Body.Close()

    header = resp.Header

    body, err = io.ReadAll(resp.Body)
    return body, header, err
}

func ParseFeed(body string) (feed *gofeed.Feed, err error) {
    fp := gofeed.NewParser()
    feed, err = fp.ParseString(body)
    // feed, err = fp.ParseURL(config.Feed.Url)
    return feed, err
}

func GetConfig() *Config {
    var configPath string
    flag.StringVar(&configPath, "config", "env.toml", "Path to the configuration file")
    flag.Parse()
    file, err := os.ReadFile(configPath)
    if err != nil {
        log.Fatal(err)
    }

    var config Config
    err = toml.Unmarshal(file, &config)
    if err != nil {
        log.Fatal(err)
    }

    log.Println("Loaded config")
    return &config
}

func truncateString(body string, maxLen int) string {
    suffix := "..."
    if len(body) <= maxLen {
        return body
    }
    return body[:maxLen-len(suffix)] + suffix
}
