package main

import (
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

const CONFIG_PATH = "env.toml"

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

var (
    dg *discordgo.Session
    config *Config
    schdl gocron.Scheduler
    visitedList []string
)
var (
    // integerOptionMinValue          = 1.0
    // dmPermission                   = false
    // defaultMemberPermissions int64 = discordgo.PermissionManageServer

    commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
        // "test": testLinkedMessage,
    }
    commands = []*discordgo.ApplicationCommand{
        // {
        //     Name: "test",
        //     Description: "test command",
        // },
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
        fmt.Println("Error opening connection,", err)
        return
    }
    defer dg.Close()
    fmt.Println("Discord Connected!")
    registeredCommands := UpDiscord()

    InitFeed()

    schdl.Start()
    log.Println("Cron Scheduler Started.")
    defer schdl.Shutdown()

    // Wait here until CTRL-C or other term signal is received.
    fmt.Println("Bot is now running. Press CTRL-C to exit.")
    sc := make(chan os.Signal, 1)
    signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
    <-sc

    DownDiscord(registeredCommands)
}

func onReady(s *discordgo.Session, event *discordgo.Ready) {
    log.Printf("Logged in as %s", event.User.String())
    s.UpdateGameStatus(0, config.DiscordBot.Status)
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
            PostFeedItem(feed, item)
        }
    }

    visitedList = make([]string,0)
    for _, item := range feed.Items {
        visitedList = append(visitedList, item.GUID)
    }
}

func PostFeedItem(feed *gofeed.Feed, item *gofeed.Item) {
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
        return
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
        return
    }
    log.Println("Created notification message", notifyMsg.ID, body)
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
            h(s, i)
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
    file, err := os.ReadFile(CONFIG_PATH)
    if err != nil {
        log.Fatal(err)
    }

    var config Config
    err = toml.Unmarshal(file, &config)
    if err != nil {
        log.Fatal(err)
    }
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
