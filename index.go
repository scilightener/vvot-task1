package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	errorResponse = "not found"

	msgHelp = "Я помогу подготовить ответ на экзаменационный вопрос по дисциплине Операционные системы.\n" +
		"Пришлите мне фотографию с вопросом или наберите его текстом."
	msgCantAnswer         = "Я не смог подготовить ответ на экзаменационный вопрос."
	msgManyPhotos         = "Я могу обработать только одну фотографию."
	msgPhotoWrongFormat   = "Я не могу обработать эту фотографию."
	msgUnknownRequestType = "Я могу обработать только текстовое сообщение или фотографию."
)

func Handle(w http.ResponseWriter, r *http.Request) {
	// TODO: make sure these are not empty
	tgAPIKey := os.Getenv("TG_BOT_KEY")
	visionAPIKey := os.Getenv("VISION_API_KEY")
	gptAPIKey := os.Getenv("YAGPT_API_KEY")
	s3StaticKey := os.Getenv("S3_STATIC_KEY")
	gptInstructionPath := os.Getenv("YAGPT_INSTRUCTION_PATH")

	bot := NewBot(tgAPIKey, visionAPIKey, gptAPIKey, s3StaticKey, gptInstructionPath)
	bot.Handle(w, r)
}

type Bot struct {
	tgAPIKey           string
	visionAPIKey       string
	gptAPIKey          string
	s3StaticKey        string
	gptInstructionPath string
}

func NewBot(tgAPIKey, visionAPIKey, gptAPIKey, s3StaticKey, gptInstructionPath string) *Bot {
	return &Bot{
		tgAPIKey:           tgAPIKey,
		visionAPIKey:       visionAPIKey,
		gptAPIKey:          gptAPIKey,
		s3StaticKey:        s3StaticKey,
		gptInstructionPath: gptInstructionPath,
	}
}

func (b *Bot) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(errorResponse))
		return
	}

	var update struct {
		Message struct {
			Text  string `json:"text"`
			Photo []struct {
				FileID string `json:"file_id"`
			} `json:"photo"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}

	err := json.NewDecoder(r.Body).Decode(&update)
	if err != nil {
		http.Error(w, "failed to parse update", http.StatusBadRequest)
		return
	}

	fmt.Println("update", update)
	switch {
	case update.Message.Text == "/start" || update.Message.Text == "/help":
		b.sendMessage(update.Message.Chat.ID, msgHelp)
	case len(update.Message.Text) > 0:
		b.processText(update.Message.Chat.ID, update.Message.Text)
	case len(update.Message.Photo) > 0:
		b.processPhoto(update.Message.Chat.ID,
			update.Message.Photo[len(update.Message.Photo)-1].FileID)
	default:
		b.sendMessage(update.Message.Chat.ID, msgUnknownRequestType)
	}
}

func (b *Bot) processText(chatID int64, text string) {
	answer, err := b.askGPT(text)
	if err != nil {
		fmt.Printf("chatID %d processText: %s\n", chatID, err.Error())
		b.sendMessage(chatID, msgCantAnswer)
		return
	}
	b.sendMessage(chatID, answer)
}

func (b *Bot) processPhoto(chatID int64, fileID string) {
	ocrURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", b.tgAPIKey, fileID)

	resp, err := http.Get(ocrURL)
	if err != nil {
		fmt.Printf("chatID %d fileID %s processPhoto: %s\n", chatID, fileID, err.Error())
		b.sendMessage(chatID, msgCantAnswer)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	filePath := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.tgAPIKey, result.Result.FilePath)
	imageContent, err := getImageBase64(filePath)
	if err != nil {
		fmt.Printf("chatID %d fileID %s processPhoto: %s\n", chatID, fileID, err.Error())
		b.sendMessage(chatID, msgCantAnswer)
		return
	}
	recognizedText, err := b.recognizeText(imageContent)
	if err != nil {
		fmt.Printf("chatID %d fileID %s processPhoto: %s\n", chatID, fileID, err.Error())
		b.sendMessage(chatID, msgCantAnswer)
		return
	}

	b.processText(chatID, recognizedText)
}

func (b *Bot) recognizeText(imageContent string) (string, error) {
	reqBody := map[string]interface{}{
		"languageCodes": []string{"*"},
		"model":         "page",
		"content":       imageContent,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Printf("recognizeText: %s\n", err.Error())
		return "", err
	}

	request, err := http.NewRequest(
		"POST",
		"https://ocr.api.cloud.yandex.net/ocr/v1/recognizeText",
		bytes.NewBuffer(reqJSON),
	)
	if err != nil {
		fmt.Printf("recognizeText: %s\n", err.Error())
		return "", err
	}
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Authorization", "Api-Key "+b.visionAPIKey)

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Printf("recognizeText: %s\n", err.Error())
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			TextAnnotation struct {
				Blocks []struct {
					Lines []struct {
						Text string `json:"text"`
					} `json:"lines"`
				} `json:"blocks"`
			} `json:"textAnnotation"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if len(result.Result.TextAnnotation.Blocks) == 0 {
		return "", fmt.Errorf("no text found")
	}

	var sb strings.Builder
	for _, b := range result.Result.TextAnnotation.Blocks {
		for _, l := range b.Lines {
			sb.WriteString(l.Text)
			sb.WriteByte('\n')
		}
	}
	return sb.String(), nil
}

func (b *Bot) askGPT(question string) (string, error) {
	// TODO: make gpt request
	return "ваш вопрос: " + question, nil
}

func (b *Bot) sendMessage(chatID int64, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.tgAPIKey)
	http.PostForm(url, map[string][]string{"chat_id": {fmt.Sprint(chatID)}, "text": {text}})
}

func getImageBase64(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("GetImageBase64: failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GetImageBase64: unexpected HTTP status: %s", resp.Status)
	}

	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("GetImageBase64: failed to read image data: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(imageData)

	return encoded, nil
}
