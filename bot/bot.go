package bot

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/jasonlvhit/gocron"
	"github.com/maddevsio/punisher/config"
	"github.com/maddevsio/punisher/model"
	"github.com/maddevsio/punisher/storage"
	"github.com/pkg/errors"
	"gopkg.in/telegram-bot-api.v4"
)

const (
	telegramAPIUpdateInterval = 60
	maxResults                = 20
	minPushUps                = 50
	maxPushUps                = 500
)

// Bot ...
type Bot struct {
	c       *config.BotConfig
	tgAPI   *tgbotapi.BotAPI
	updates tgbotapi.UpdatesChannel
	db      *storage.MySQL
}

// NewTGBot creates a new bot
func NewTGBot(c *config.BotConfig) (*Bot, error) {
	newBot, err := tgbotapi.NewBotAPI(c.TelegramToken)
	if err != nil {
		return nil, err
	}
	b := &Bot{
		c:     c,
		tgAPI: newBot,
	}
	u := tgbotapi.NewUpdate(0)
	u.Timeout = telegramAPIUpdateInterval
	updates, err := b.tgAPI.GetUpdatesChan(u)
	if err != nil {
		return nil, err
	}
	conn, err := storage.NewMySQL(c)
	if err != nil {
		return nil, err
	}
	b.updates = updates
	b.db = conn
	gocron.Every(1).Day().At(c.PunishTime).Do(b.dailyJob)

	return b, nil
}

// Start ...
func (b *Bot) Start() {
	go func() {
		<-gocron.Start()
	}()
	log.Println("Starting tg bot")
	for update := range b.updates {
		b.handleUpdate(update)
	}
}

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}
	text := update.Message.Text
	if text == "" || text == "/start" {
		return
	}

	isAdmin, err := b.senderIsAdminInChannel(update.Message.From.UserName)
	if err != nil {
		fmt.Println("WHO THE HELL IS THIS GUY?")
	}
	s := strings.Split(update.Message.Text, " ")
	fmt.Println(s)
	if s[0] == "@internshipcomedian_bot" && s[0] != "" && s[1] != "" && s[2] != "" && isAdmin {
		switch c := s[1]; c {
		case "добавь":
			fmt.Printf("Добавляю стажёра: %s в БД", s[2])
			intern := model.Intern{0, s[2], 3}
			_, err := b.db.CreateIntern(intern)
			if err != nil {
				fmt.Println(err)
				return
			}
		case "удали":
			fmt.Printf("Удаляю стажёра: %s из БД", s[2])
			intern, err := b.db.FindIntern(s[2])
			if err != nil {
				fmt.Println(err)
				return
			}
			err = b.db.DeleteIntern(intern.ID)
			if err != nil {
				fmt.Println(err)
				return
			}
		}
	}

	if b.isStandup(update.Message) {
		fmt.Printf("accepted standup from %s\n", update.Message.From.UserName)
		standup := model.Standup{
			Comment:  update.Message.Text,
			Username: update.Message.From.UserName,
		}
		_, err := b.db.CreateStandup(standup)
		if err != nil {
			log.Println(err)
			return
		}
		b.tgAPI.Send(tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s спасибо. Я принял твой стендап", update.Message.From.UserName)))

		if b.c.NotifyMentors {
			b.tgAPI.Send(tgbotapi.ForwardConfig{
				FromChannelUsername: update.Message.From.UserName,
				FromChatID:          -b.c.InternsChatID,
				MessageID:           update.Message.MessageID,
				BaseChat:            tgbotapi.BaseChat{ChatID: b.c.MentorsChat},
			})
		}

	}

	if update.EditedMessage != nil {
		if !b.isStandup(update.EditedMessage) {
			fmt.Printf("This is not a proper edit for standup: %s\n", update.EditedMessage.Text)
			return
		}
		fmt.Printf("accepted edited standup from %s\n", update.EditedMessage.From.UserName)
		standup := model.Standup{
			Comment:  update.EditedMessage.Text,
			Username: update.EditedMessage.From.UserName,
		}
		_, err := b.db.UpdateStandup(standup)
		if err != nil {
			log.Println(err)
			return
		}
		b.tgAPI.Send(tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s спасибо. исправления приняты.", update.Message.From.UserName)))
	}
}

func (b *Bot) dailyJob() {
	if _, err := b.checkStandups(); err != nil {
		log.Println(err)
	}
}

func (b *Bot) senderIsAdminInChannel(sendername string) (bool, error) {
	isAdmin := false
	chat := tgbotapi.ChatConfig{b.c.InternsChatID, ""}
	admins, err := b.tgAPI.GetChatAdministrators(chat)
	if err != nil {
		return false, err
	}
	for _, admin := range admins {
		if admin.User.UserName == sendername {
			isAdmin = true
			return true, nil
		}
	}
	return isAdmin, nil
}

func (b *Bot) checkStandups() (string, error) {
	if time.Now().Weekday().String() == "Saturday" || time.Now().Weekday().String() == "Sunday" {
		return "", errors.New("day off")
	}
	interns, err := b.db.ListInterns()
	if err != nil {
		return "", err
	}
	for _, intern := range interns {
		conf := tgbotapi.ChatConfigWithUser{
			ChatID: b.c.InternsChatID,
			UserID: int(intern.ID),
		}
		u, err := b.tgAPI.GetChatMember(conf)
		if err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Println(u)
		standup, err := b.db.LastStandupFor(intern.Username)
		if err != nil {
			if err == sql.ErrNoRows {
				b.Punish(intern)
				continue
			} else {
				return "", err
			}
		}
		t, _ := time.LoadLocation("Asia/Bishkek")
		if time.Now().Day() != standup.Created.In(t).Day() {
			b.Punish(intern)
		}
	}
	message := tgbotapi.NewMessage(-b.c.InternsChatID, "Каратель завершил свою работу ;)")
	b.tgAPI.Send(message)
	return message.Text, nil
}

func (b *Bot) isStandup(message *tgbotapi.Message) bool {
	log.Println("checking accepted message")
	var mentionsProblem, mentionsYesterdayWork, mentionsTodayPlans bool

	problemKeys := []string{"роблем", "рудност", "атрдуднен"}
	for _, problem := range problemKeys {
		if strings.Contains(message.Text, problem) {
			mentionsProblem = true
		}
	}

	yesterdayWorkKeys := []string{"чера", "ятницу", "делал", "делано"}
	for _, work := range yesterdayWorkKeys {
		if strings.Contains(message.Text, work) {
			mentionsYesterdayWork = true
		}
	}

	todayPlansKeys := []string{"егодн", "обираюс", "ланир"}
	for _, plan := range todayPlansKeys {
		if strings.Contains(message.Text, plan) {
			mentionsTodayPlans = true
		}
	}
	return mentionsProblem && mentionsYesterdayWork && mentionsTodayPlans
}

//RemoveLives removes live from intern
func (b *Bot) RemoveLives(intern model.Intern) (string, error) {
	intern.Lives--
	_, err := b.db.UpdateIntern(intern)
	if err != nil {
		return "", err
	}
	message := tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s осталось жизней: %d", intern.Username, intern.Lives))
	if intern.Lives == 0 {
		chatMemberConf := tgbotapi.ChatMemberConfig{
			ChatID: b.c.InternsChatID,
			UserID: int(intern.ID),
		}
		conf := tgbotapi.KickChatMemberConfig{chatMemberConf, 0}
		r, err := b.tgAPI.KickChatMember(conf)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(r)
		message = tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("У @%s не осталось жизней. Удаляю.", intern.Username))
	}
	b.tgAPI.Send(message)
	return message.Text, nil
}

//PunishByPushUps tells interns to do random # of pushups
func (b *Bot) PunishByPushUps(intern model.Intern, min, max int) (int, string, error) {
	rand.Seed(time.Now().Unix())
	pushUps := rand.Intn(max-min) + min
	message := tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s в наказание за пропущенный стэндап тебе %d отжиманий", intern.Username, pushUps))
	b.tgAPI.Send(message)
	return pushUps, message.Text, nil
}

//PunishBySitUps tells interns to do random # of pushups
func (b *Bot) PunishBySitUps(intern model.Intern, min, max int) (int, string, error) {
	rand.Seed(time.Now().Unix())
	situps := rand.Intn(max-min) + min
	message := tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s в наказание за пропущенный стэндап тебе %d приседаний", intern.Username, situps))
	b.tgAPI.Send(message)
	return situps, message.Text, nil
}

//PunishByPoetry tells interns to read random poetry
func (b *Bot) PunishByPoetry(intern model.Intern, link string) (string, string, error) {
	message := tgbotapi.NewMessage(-b.c.InternsChatID, fmt.Sprintf("@%s в наказание за пропущенный стэндап прочитай этот стих: %v", intern.Username, link))
	b.tgAPI.Send(message)
	return link, message.Text, nil
}

//Punish punishes interns by either removing lives or asking them to do push ups
func (b *Bot) Punish(intern model.Intern) {
	switch punishment := b.c.PunishmentType; punishment {
	case "pushups":
		b.PunishByPushUps(intern, minPushUps, maxPushUps)
	case "removelives":
		b.RemoveLives(intern)
	case "situps":
		b.PunishBySitUps(intern, minPushUps, maxPushUps)
	case "poetry":
		link := generatePoetryLink()
		b.PunishByPoetry(intern, link)
	default:
		b.randomPunishment(intern)
	}
}

func (b *Bot) randomPunishment(intern model.Intern) {
	rand.Seed(time.Now().Unix())
	switch r := rand.Intn(3); r {
	case 0:
		b.PunishByPushUps(intern, minPushUps, maxPushUps)
	case 1:
		b.RemoveLives(intern)
	case 2:
		b.PunishBySitUps(intern, minPushUps, maxPushUps)
	case 3:
		link := generatePoetryLink()
		b.PunishByPoetry(intern, link)
	}
}

func generatePoetryLink() string {
	rand.Seed(time.Now().Unix())
	year := rand.Intn(2018-2008) + 2008
	month := rand.Intn(12-1) + 1
	date := rand.Intn(30-1) + 1
	id := rand.Intn(4100-10) + 10
	link := fmt.Sprintf("https://www.stihi.ru/%04d/%02d/%02d/%v", year, month, date, id)
	if !poetryExist(link) {
		generatePoetryLink()
	}
	return link
}

//poetryExist checks if link leads to real poetry and not 404 error
func poetryExist(link string) bool {
	resp, err := http.Get(link)
	if err != nil {
		fmt.Println(err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		if strings.Contains(bodyString, "404:") {
			return false
		}
		return true
	}

	return false
}
