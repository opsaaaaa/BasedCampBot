package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/go-co-op/gocron/v2"
	"github.com/mmcdole/gofeed"
	"github.com/pelletier/go-toml/v2"
)

type Config struct {
    RemoveCommands bool
    Feed struct {
        Url string
        Type string
        CronSchedule string
    }
    DiscordBot struct {
        Username string
        ClientID string
        ClientSecret string
        Token string
        Status string
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

type FeedResult struct {
    Feed *gofeed.Feed
    Item *gofeed.Item
}


var (
    dg *discordgo.Session
    config *Config
    schdl gocron.Scheduler
    visitedList []string
)
var (
    integerOptionMinValue          = 1.0
    // dmPermission                   = false
    // defaultMemberPermissions int64 = discordgo.PermissionManageServer

    commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate) error {
        // "test": testLinkedMessage,
        "ping": cmdPingpong,
        "checkfeed": cmdCheckfeed,
        "checkconfig": cmdCheckConfig,
        "postlatest": cmdPostlatest,
        "postnew": cmdPostNewFeed,
    }
    commands = []*discordgo.ApplicationCommand{
        // {
        //     Name: "test",
        //     Description: "test command",
        // },
        {
            Name: "ping",
            Description: "pong",
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

func main() {
    err := dg.Open()
    if err != nil {
        log.Println("Error opening connection,", err)
        return
    }
    defer dg.Close()
    log.Println("Discord Connected!")
    registeredCommands := UpDiscord()

    InitFeed()

    schdl.Start()
    log.Println("Cron Scheduler Started.")
    defer schdl.Shutdown()

    // Wait here until CTRL-C or other term signal is received.
    log.Println("Bot is now running. Press CTRL-C to exit.")
    sc := make(chan os.Signal, 1)
    signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
    <-sc

    DownDiscord(registeredCommands)
}

func onReady(s *discordgo.Session, event *discordgo.Ready) {
    log.Printf("Logged in as %s", event.User.String())
    _ = s.UpdateGameStatus(0, config.DiscordBot.Status)
}

func onCronCallback() {
    log.Println("Cron Callback")
    feed, err := GetFeed()
    if err != nil {
        log.Println("Error getting feed.", err)
        return
    }

    for _, item := range feed.Items {
        if !containsString(visitedList, item.GUID) {
            _ = PostFeedItem(feed, item)
        }
    }

    visitedList = make([]string,0)
    for _, item := range feed.Items {
        visitedList = append(visitedList, item.GUID)
    }
}


func QueryAllFeedItems() (*gofeed.Feed, error) {
    feed, err := GetFeed()
    if err != nil {
        log.Println("Error getting feed.", err)
        return feed, err
    }

    visitedList = make([]string,0)
    for _, item := range feed.Items {
        visitedList = append(visitedList, item.GUID)
    }
    return feed, nil
}

func QueryNewFeedItems() ([]FeedResult, error) {
    result := make([]FeedResult, 0)
    feed, err := GetFeed()
    if err != nil {
        log.Println("Error getting feed.", err)
        return result, err
    }

    for _, item := range feed.Items {
        if !containsString(visitedList, item.GUID) {
            result = append(result, FeedResult{
                Feed: feed,
                Item: item,
            })
        }
    }

    visitedList = make([]string,0)
    for _, item := range feed.Items {
        visitedList = append(visitedList, item.GUID)
    }
    return result, nil
}

func PostFeedItem(feed *gofeed.Feed, item *gofeed.Item) error {
    log.Println("Posting.",item.GUID, item.Title)

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
        log.Println("Error making ForumThread post.", err, title, body)
        return err
    }
    log.Println("Created ForumThread post.", postMsg.ID, title, body)

    body = truncateString(
        config.DiscordMsg.NotifyPrefix + " " +
        "https://discord.com/channels/"+config.DiscordServer.GuildID+"/"+postMsg.ParentID+"/"+postMsg.ID+"\n"+
        item.Title,
        config.Discord.MaxMessageLength,
    )

    notifyMsg, err := dg.ChannelMessageSend(config.DiscordServer.NotifyChannelID, body)

    if err != nil {
        log.Println("Error sending discord notify message.",err, body)
        return err
    }
    log.Println("Created notification message", notifyMsg.ID, body)
    return nil
}


func InitFeed() {
    feed, err := GetFeed()
    if err != nil {
        log.Fatalln("Error getting feed.", err)
    }
    visitedList = make([]string,0)
    for _, item := range feed.Items {
        visitedList = append(visitedList, item.GUID)
    }
    log.Println("Added visitedList.", visitedList)
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
            err = h(s, i)
            if err != nil {
                log.Println("Error "+i.ApplicationCommandData().Name, err)
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
    log.Println("Registered Discord Commands")
    return registeredCommands
}

func DownDiscord(registeredCommands []*discordgo.ApplicationCommand) {
    if config.RemoveCommands {
        log.Println("Removing commands...")
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
    log.Printf("%s ran ./postlatest", i.User.Username)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdCheckConfig(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var content string
    content += fmt.Sprintf("Post to https://discord.com/channels/%s/%s\n", config.DiscordServer.GuildID, config.DiscordServer.PostChannelID)
    content += fmt.Sprintf("Notify to https://discord.com/channels/%s/%s\n", config.DiscordServer.GuildID, config.DiscordServer.NotifyChannelID)
    content += fmt.Sprintf("Feed Source `%s`\n", config.Feed.Url)
    content += fmt.Sprintf("Cron Schedule `%s`\n", config.Feed.CronSchedule)
    content += fmt.Sprintf("Notify Prefix `%s`\n", config.DiscordMsg.NotifyPrefix)
    content += fmt.Sprintf("TimeFormat `%s`\n", config.DiscordMsg.TimeFormat)
    log.Printf("%s ran ./checkconfig", i.User.Username)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdCheckfeed(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var maxCount = 3
    for _, opt := range i.ApplicationCommandData().Options {
        switch opt.Name {
            case "count":
                maxCount = int(opt.IntValue())
            default:
        }
    }
    var content string
    feed, err := GetFeed()
    if err != nil {
        content = "Can't query feed '"+config.Feed.Url+"'."
    } else if len(feed.Items) > 0 {
        for x, item := range feed.Items {
            if x >= maxCount {
                break
            }
            checkbox := "✅"
            if !containsString(visitedList, item.GUID) {
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
    log.Printf("%s ran ./checkfeed", i.User.Username)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdPostNewFeed(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    var content string
    feedResults, err := QueryNewFeedItems()
    if err != nil {
        content = "Can't query feed '"+config.Feed.Url+"'."
    } else if len(feedResults) > 0 {
        for _, feedResult := range feedResults {
            err = PostFeedItem(feedResult.Feed, feedResult.Item)
            if err != nil {
                content = content + "Failed '"+ feedResult.Item.Title + "'.\n"
            } else {
                content = content + "Posted '"+ feedResult.Item.Title + "'.\n"
            }
        }
    } else {
        content = "No new items in feed to post."
    }
    log.Printf("%s ran ./postnew with %d items", i.User.Username, len(feedResults))
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: content,
        },
    })
}

func cmdPingpong(s *discordgo.Session, i *discordgo.InteractionCreate) error {
    log.Printf("%s ran ./ping", i.User.Username)
    return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Pong!",
        },
    })
}

// func testLinkedMessage(s *discordgo.Session, i *discordgo.InteractionCreate) {
//     s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
//         Type: discordgo.InteractionResponseChannelMessageWithSource,
//         Data: &discordgo.InteractionResponseData{
//             Content: "Hey there! Congratulations, you just executed your first slash command",
//         },
//     })
//     msg, err := s.ChannelMessageSend(config.DiscordServer.PostChannelID, "Test Post ...")
//     if err != nil {
//         log.Println("error sending discord post message.",err)
//         return
//     }
//     msg, err = s.ChannelMessageSend(config.DiscordServer.NotifyChannelID, 
//         "Test Notify https://discord.com/channels/"+
//         config.DiscordServer.GuildID+
//         "/"+msg.ChannelID+"/"+msg.ID,
//     )
//     if err != nil {
//         log.Println("error sending discord notify message.",err)
//         return
//     }
// }

func GetFeed() (*gofeed.Feed, error) {
    fp := gofeed.NewParser()
    feed, err := fp.ParseURL(config.Feed.Url)
    if err != nil {
        log.Println(err)
    }
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

func containsString(haystack []string, needle string) bool {
    for _, v := range haystack {
        if v == needle {
            return true
        }
    }
    return false
}

func truncateString(body string, maxLen int) string {
    suffix := "..."
    if len(body) <= maxLen {
        return body
    }
    return body[:maxLen-len(suffix)] + suffix
}
