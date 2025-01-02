package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
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
	folderID := os.Getenv("FOLDER_ID")
	s3AccessKey := os.Getenv("S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("S3_SECRET_KEY")
	gptInstructionPath := os.Getenv("YAGPT_INSTRUCTION_PATH")

	bot := NewBot(tgAPIKey, visionAPIKey, gptAPIKey, folderID, s3AccessKey, s3SecretKey, gptInstructionPath)
	bot.Handle(w, r)
}

type Bot struct {
	tgAPIKey           string
	visionAPIKey       string
	gptAPIKey          string
	folderID           string
	storageAccessKey   string
	storageSecretKey   string
	gptInstructionPath string
}

func NewBot(tgAPIKey, visionAPIKey, gptAPIKey, folderID, storageAccessKey, storageSecretKey, gptInstructionPath string) *Bot {
	return &Bot{
		tgAPIKey:           tgAPIKey,
		visionAPIKey:       visionAPIKey,
		gptAPIKey:          gptAPIKey,
		folderID:           folderID,
		storageAccessKey:   storageAccessKey,
		storageSecretKey:   storageSecretKey,
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

func (b *Bot) sendMessage(chatID int64, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.tgAPIKey)
	http.PostForm(url, map[string][]string{"chat_id": {fmt.Sprint(chatID)}, "text": {text}})
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
		return "", fmt.Errorf("recognizeText: %w", err)
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
		return "", fmt.Errorf("recognizeText: %w", err)
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
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", fmt.Errorf("recognizeText: %w", err)
	}

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
	systemPropt, err := b.getGPTInstructions()
	if err != nil {
		return "", fmt.Errorf("askGPT: %w", err)
	}

	reqBody := map[string]interface{}{
		"modelUri": fmt.Sprintf("gpt://%s/yandexgpt", b.folderID),
		"completionOptions": map[string]interface{}{
			"stream":      false,
			"temperature": 0.1,
			"maxTokens":   "1000",
		},
		"messages": []map[string]interface{}{
			{
				"role": "system",
				"text": systemPropt,
			},
			{
				"role": "user",
				"text": question,
			},
		},
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("askGPT: %w", err)
	}

	const yagptAPI = "https://llm.api.cloud.yandex.net/foundationModels/v1/completion"
	request, err := http.NewRequest("POST", yagptAPI, bytes.NewBuffer(reqJSON))
	if err != nil {
		return "", fmt.Errorf("askGPT: %w", err)
	}
	request.Header.Add("Authorization", "Api-Key "+b.gptAPIKey)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("askGPT: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			Alternatives []struct {
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"alternatives"`
		} `json:"result"`
	}

	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", fmt.Errorf("askGPT: %w", err)
	}
	if len(result.Result.Alternatives) < 1 {
		return "", fmt.Errorf("can't answer")
	}
	return result.Result.Alternatives[0].Message.Text, nil
}

func (b *Bot) getGPTInstructions() (string, error) {
	region := "ru-central1"
	service := "s3"
	bucket, objectKey, err := extractBucketAndKey(b.gptInstructionPath)
	if err != nil {
		return "", fmt.Errorf("getGPTInstructions: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	headers := map[string]string{
		"Host":                 "storage.yandexcloud.net",
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": "UNSIGNED-PAYLOAD",
	}

	canonicalHeaders := "host:storage.yandexcloud.net\nx-amz-content-sha256:UNSIGNED-PAYLOAD\nx-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("GET\n/%s/%s\n\n%s\n%s\nUNSIGNED-PAYLOAD", bucket, objectKey, canonicalHeaders, signedHeaders)

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, region, service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%x", amzDate, scope, sha256.Sum256([]byte(canonicalRequest)))

	signingKey := getSignatureKey(b.storageSecretKey, date, region, service)
	signature := hex.EncodeToString(sign(signingKey, stringToSign))

	headers["Authorization"] = fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.storageAccessKey, scope, signedHeaders, signature,
	)

	req, err := http.NewRequest("GET", b.gptInstructionPath, nil)
	if err != nil {
		return "", fmt.Errorf("getGPTInstructions: %v", err)
	}

	for key, value := range headers {
		req.Header.Add(key, value)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("getGPTInstructions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("getGPTInstructions: unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("getGPTInstructions: %v", err)
	}

	return string(body), nil
}

// sign генерирует подпись HMAC.
func sign(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	return h.Sum(nil)
}

// getSignatureKey генерирует ключ для подписи.
func getSignatureKey(secretKey, date, region, service string) []byte {
	kDate := sign([]byte("AWS4"+secretKey), date)
	kRegion := sign(kDate, region)
	kService := sign(kRegion, service)
	kSigning := sign(kService, "aws4_request")
	return kSigning
}

func extractBucketAndKey(rawURL string) (string, string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %v", err)
	}

	pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(pathParts) < 2 {
		return "", "", fmt.Errorf("URL does not contain both bucket name and object key")
	}

	bucketName := pathParts[0]
	objectKey := strings.Join(pathParts[1:], "/")
	return bucketName, objectKey, nil
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
