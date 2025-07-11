package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/sashabaranov/go-openai"

	"LingChat/api"
	"LingChat/api/routes/v1/response"
	"LingChat/internal/clients/VitsTTS"
	"LingChat/internal/clients/emotionPredictor"
	"LingChat/internal/clients/llm"
)

type LingChatService struct {
	emotionPredictorClient *emotionPredictor.Client
	VitsTTSClient          *VitsTTS.Client
	llmClient              *llm.LLMClient
	conversationService    *ConversationService
	ConfigModel            string
	tempFilePath           string
}

func NewLingChatService(
	epClient *emotionPredictor.Client,
	vtClient *VitsTTS.Client,
	llmClient *llm.LLMClient,
	conversationService *ConversationService,
	configModel string,
	path string,
) *LingChatService {

	return &LingChatService{
		emotionPredictorClient: epClient,
		VitsTTSClient:          vtClient,
		llmClient:              llmClient,
		conversationService:    conversationService,
		ConfigModel:            configModel,
		tempFilePath:           path,
	}
}

func (l *LingChatService) EmoPredictBatch(ctx context.Context, results []Result) []Result {
	var wg sync.WaitGroup
	resultsChannel := make(chan struct {
		index      int
		Predicted  string
		Confidence float64
	}, len(results))
	for i, result := range results {
		wg.Add(1)
		go func(index int, result Result) {
			defer wg.Done()
			resp, err := l.emotionPredictorClient.Predict(ctx, result.OriginalTag, 0.08)
			if err != nil {
				log.Printf("Failed to predict emotion: %v", err)
				resultsChannel <- struct {
					index      int
					Predicted  string
					Confidence float64
				}{
					index, "unknown", 0.0,
				}
			} else {
				resultsChannel <- struct {
					index      int
					Predicted  string
					Confidence float64
				}{
					index, resp.Label, resp.Confidence,
				}
			}
		}(i, result)
	}

	go func() {
		wg.Wait()
		close(resultsChannel)
	}()

	for result := range resultsChannel {
		index := result.index
		results[index].Confidence = result.Confidence
		results[index].Predicted = result.Predicted
	}
	return results
}

func (l *LingChatService) LingChatByWS(ctx context.Context, msg api.Message) ([]api.Response, error) {
	switch msg.Type {
	case "message":
	case "handshake":
		log.Printf("handshake with message:\"%s\"\n", msg.Content)
		return nil, nil
	case "ping":
		log.Println("Ping received, Pong")
		return nil, nil
	default:
		return nil, fmt.Errorf("invalid type \"%s\" with message: \"%s\"", msg.Type, msg.Content)
	}

	resp, err := l.LingChat(ctx, msg.Content, "", "")
	if err != nil {
		return nil, err
	}

	return resp.Messages, nil
}

func (l *LingChatService) LingChat(ctx context.Context, message string, conversationID, prevMessageID string) (*response.CompletionResponse, error) {
	cleanTempVoiceFiles(l.tempFilePath)

	// 记录会话和消息
	conv, userMsgObj, err := l.conversationService.RecordConversationAndMessage(ctx, message, conversationID, prevMessageID)
	if err != nil {
		return nil, err
	}

	// 获取消息链
	messages, err := l.conversationService.GetChatContext(ctx, userMsgObj.ID)
	if err != nil {
		return nil, err
	}

	// 调用LLM获取回复
	rawLLMResp, err := l.llmClient.Chat(ctx, messages, l.ConfigModel)
	if err != nil {
		err = fmt.Errorf("LLM Chat error: %w", err)
		return nil, err
	}

	// 将助手回复保存到数据库
	respMsg, err := l.conversationService.SaveAssistantMessage(ctx, userMsgObj.ID, rawLLMResp)
	if err != nil {
		log.Printf("保存助手回复失败: %s", err)
	}

	emotionSegments := AnalyzeEmotions(rawLLMResp, l.tempFilePath, "wav")

	// TODO: 这里两条会耦合使用emotionSegments的字段，后面要改
	_, err = l.GenerateVoice(ctx, emotionSegments, true)
	if err != nil {
		log.Printf("GenerateVoice error: %s", err)
	}
	emotionSegments = l.EmoPredictBatch(ctx, emotionSegments)

	return &response.CompletionResponse{
		ConversationID: strconv.Itoa(int(conv.ID)),
		MessageID:      strconv.Itoa(int(respMsg.ID)),
		Messages:       l.CreateResponse(emotionSegments, message),
	}, nil
}

func (l *LingChatService) CreateResponse(results []Result, userMessage string) []api.Response {
	var resp []api.Response
	for i, result := range results {
		resp = append(resp, api.Response{
			Type:            "reply",
			Emotion:         result.Predicted,
			OriginalTag:     result.OriginalTag,
			Message:         result.FollowingText,
			MotionText:      result.MotionText,
			AudioFile:       filepath.Base(result.VoiceFile),
			OriginalMessage: userMessage,
			IsMultiPart:     true,
			PartIndex:       i,
			TotalParts:      len(results),
		})
	}
	return resp
}

func (l *LingChatService) GenerateVoice(ctx context.Context, textSegments []Result, saveFile bool) ([][]byte, error) {
	// 创建一个带缓冲的通道来收集结果
	results := make(chan struct {
		index int
		data  []byte
		err   error
	}, len(textSegments))

	// 创建 WaitGroup
	var wg sync.WaitGroup
	wg.Add(len(textSegments))

	// 为每个文本片段启动一个goroutine
	for i, segment := range textSegments {
		go func(idx int, text string) {
			defer wg.Done()
			// 调用VITS TTS服务生成语音
			audioData, err := l.VitsTTSClient.VoiceVITS(ctx, text)
			results <- struct {
				index int
				data  []byte
				err   error
			}{idx, audioData, err}
		}(i, segment.JapaneseText)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集所有结果
	audioDataList := make([][]byte, len(textSegments))
	var firstErr error

	// 从通道中读取结果
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		audioDataList[result.index] = result.data

		// 如果保存文件，将音频数据写入文件
		if saveFile && len(result.data) != 0 {
			voiceFile := textSegments[result.index].VoiceFile
			// 确保目录存在
			dir := filepath.Dir(voiceFile)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("Failed to create directory %s: %v", dir, err)
				continue
			}

			// 写入文件
			if err := os.WriteFile(voiceFile, result.data, 0644); err != nil {
				log.Printf("Failed to write file %s: %v", voiceFile, err)
				continue
			}
		}
	}

	return audioDataList, firstErr
}

func cleanTempVoiceFiles(tempVoiceDir string) {
	// 检查目录是否存在
	if _, err := os.Stat(tempVoiceDir); err == nil {
		// 获取所有.wav文件
		wavFiles, err := filepath.Glob(filepath.Join(tempVoiceDir, "*.wav"))
		if err != nil {
			fmt.Printf("查找wav文件时出错: %v\n", err)
			return
		}

		// 删除每个文件
		for _, file := range wavFiles {
			if err := os.Remove(file); err != nil {
				fmt.Printf("删除文件 %s 时出错: %v\n", file, err)
			}
		}
	}
}

func (l *LingChatService) ChatHandler(rawMsg []byte) ([]api.Sentence, error) {
	var msg api.Message
	err := json.Unmarshal(rawMsg, &msg)
	if err != nil {
		err = fmt.Errorf("JSON 解析错误: %w", err)
		log.Println(err)
		return nil, err
	}

	resp, err := l.LingChatByWS(context.Background(), msg)
	if err != nil {
		err = fmt.Errorf("LingChat error: %w", err)
		log.Println(err)
		return nil, err
	}

	var respSentences []api.Sentence
	for _, msg := range resp {
		msgJSON, err := json.Marshal(msg)
		if err != nil {
			err = fmt.Errorf("JSON 序列化错误: %w", err)
			log.Println(err)
		}
		respSentences = append(respSentences, msgJSON)
	}

	return respSentences, nil
}

func (l *LingChatService) GetChatHistory(ctx context.Context) []openai.ChatCompletionMessage {
	return l.conversationService.GetChatHistory(ctx)
}

func (l *LingChatService) LoadChatHistory(ctx context.Context, msg []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	return l.conversationService.LoadChatHistory(ctx, msg)
}
