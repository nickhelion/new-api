package dify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	openaiHelper "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func downloadRemoteImage(imageUrl string) ([]byte, string, error) {
	client := service.GetHttpClient()
	resp, err := client.Get(imageUrl)
	if err != nil {
		return nil, "", fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("failed to download image, status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image body: %w", err)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" || !strings.HasPrefix(mimeType, "image/") {
		mimeType = "image/png"
	}

	return data, mimeType, nil
}

func uploadImageDataToDify(info *relaycommon.RelayInfo, user string, imageData []byte, mimeType string) *DifyFile {
	uploadUrl := fmt.Sprintf("%s/v1/files/upload", info.ChannelBaseUrl)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("user", user); err != nil {
		common.SysLog("failed to add user field: " + err.Error())
		return nil
	}

	ext := strings.TrimPrefix(mimeType, "image/")
	part, err := writer.CreateFormFile("file", fmt.Sprintf("image.%s", ext))
	if err != nil {
		common.SysLog("failed to create form file: " + err.Error())
		return nil
	}

	if _, err = io.Copy(part, bytes.NewReader(imageData)); err != nil {
		common.SysLog("failed to copy file content: " + err.Error())
		return nil
	}
	writer.Close()

	req, err := http.NewRequest("POST", uploadUrl, body)
	if err != nil {
		common.SysLog("failed to create request: " + err.Error())
		return nil
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", info.ApiKey))

	client := service.GetHttpClient()
	resp, err := client.Do(req)
	if err != nil {
		common.SysLog("failed to send request: " + err.Error())
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Id string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		common.SysLog("failed to decode response: " + err.Error())
		return nil
	}

	return &DifyFile{
		UploadFileId: result.Id,
		Type:         "image",
		TransferMode: "local_file",
	}
}

func uploadDifyFile(c *gin.Context, info *relaycommon.RelayInfo, user string, media dto.MediaContent) *DifyFile {
	switch media.Type {
	case dto.ContentTypeImageURL:
		imageMedia := media.GetImageMedia()

		if imageMedia.IsRemoteImage() {
			// Download remote image then upload to Dify
			imageData, mimeType, err := downloadRemoteImage(imageMedia.Url)
			if err != nil {
				common.SysLog("dify: " + err.Error())
				return nil
			}
			return uploadImageDataToDify(info, user, imageData, mimeType)
		}

		// Base64 image: decode then upload
		base64Data := imageMedia.Url
		if idx := strings.Index(base64Data, ","); idx != -1 {
			base64Data = base64Data[idx+1:]
		}

		decodedData, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			common.SysLog("failed to decode base64: " + err.Error())
			return nil
		}

		mimeType := imageMedia.MimeType
		if mimeType == "" {
			mimeType = "image/jpeg"
		}

		return uploadImageDataToDify(info, user, decodedData, mimeType)
	}
	return nil
}

func requestOpenAI2Dify(c *gin.Context, info *relaycommon.RelayInfo, request dto.GeneralOpenAIRequest) *DifyChatRequest {
	difyReq := DifyChatRequest{
		Inputs:           make(map[string]interface{}),
		AutoGenerateName: false,
	}

	user := request.User
	if len(user) == 0 {
		user = json.RawMessage(helper.GetResponseID(c))
	}
	var stringUser string
	err := json.Unmarshal(user, &stringUser)
	if err != nil {
		common.SysLog("failed to unmarshal user: " + err.Error())
		stringUser = helper.GetResponseID(c)
	}
	difyReq.User = stringUser

	files := make([]DifyFile, 0)
	var content strings.Builder
	for _, message := range request.Messages {
		if message.Role == "system" {
			content.WriteString("SYSTEM: \n" + message.StringContent() + "\n")
		} else if message.Role == "assistant" {
			content.WriteString("ASSISTANT: \n" + message.StringContent() + "\n")
		} else {
			parseContent := message.ParseContent()
			for _, mediaContent := range parseContent {
				switch mediaContent.Type {
				case dto.ContentTypeText:
					content.WriteString("USER: \n" + mediaContent.Text + "\n")
				case dto.ContentTypeImageURL:
					file := uploadDifyFile(c, info, difyReq.User, mediaContent)
					if file != nil {
						files = append(files, *file)
					}
				}
			}
		}
	}
	difyReq.Query = content.String()
	difyReq.Files = files
	mode := "blocking"
	if lo.FromPtrOr(request.Stream, false) {
		mode = "streaming"
	}
	difyReq.ResponseMode = mode
	return &difyReq
}

func streamResponseDify2OpenAI(difyResponse DifyChunkChatCompletionResponse) *dto.ChatCompletionsStreamResponse {
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "dify",
	}
	var choice dto.ChatCompletionsStreamResponseChoice
	if strings.HasPrefix(difyResponse.Event, "workflow_") {
		if constant.DifyDebug {
			text := "Workflow: " + difyResponse.Data.WorkflowId
			if difyResponse.Event == "workflow_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
		}
	} else if strings.HasPrefix(difyResponse.Event, "node_") {
		if constant.DifyDebug {
			text := "Node: " + difyResponse.Data.NodeType
			if difyResponse.Event == "node_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
		}
	} else if difyResponse.Event == "message" || difyResponse.Event == "agent_message" {
		if difyResponse.Answer == "<details style=\"color:gray;background-color: #f8f8f8;padding: 8px;border-radius: 4px;\" open> <summary> Thinking... </summary>\n" {
			difyResponse.Answer = "<think>"
		} else if difyResponse.Answer == "</details>" {
			difyResponse.Answer = "</think>"
		}

		choice.Delta.SetContentString(difyResponse.Answer)
	}
	response.Choices = append(response.Choices, choice)
	return &response
}

func difyStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var responseText string
	usage := &dto.Usage{}
	var nodeToken int
	var lastStreamData string
	helper.SetEventStreamHeaders(c)
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		var difyResponse DifyChunkChatCompletionResponse
		if err := common.Unmarshal([]byte(data), &difyResponse); err != nil {
			common.SysLog("error unmarshalling stream response: " + err.Error())
			sr.Error(err)
			return
		}
		if difyResponse.Event == "message_end" {
			usage = &difyResponse.MetaData.Usage
			sr.Done()
			return
		} else if difyResponse.Event == "error" {
			sr.Stop(fmt.Errorf("dify error event"))
			return
		}
		openaiResponse := *streamResponseDify2OpenAI(difyResponse)
		if len(openaiResponse.Choices) != 0 {
			responseText += openaiResponse.Choices[0].Delta.GetContentString()
			if openaiResponse.Choices[0].Delta.ReasoningContent != nil {
				nodeToken += 1
			}
		}
		// Delay-by-one pattern: send the previous chunk via HandleStreamFormat,
		// save the current chunk for the next iteration or HandleFinalResponse.
		if lastStreamData != "" {
			if err := openaiHelper.HandleStreamFormat(c, info, lastStreamData, false, false); err != nil {
				common.SysLog(err.Error())
				sr.Error(err)
			}
		}
		jsonBytes, err := common.Marshal(openaiResponse)
		if err != nil {
			common.SysLog(err.Error())
			sr.Error(err)
			return
		}
		lastStreamData = string(jsonBytes)
	})
	openaiHelper.HandleFinalResponse(c, info, lastStreamData, "", 0, info.UpstreamModelName, "", usage, usage.TotalTokens > 0)
	if usage.TotalTokens == 0 {
		usage = service.ResponseText2Usage(c, responseText, info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	usage.CompletionTokens += nodeToken
	return usage, nil
}

func difyHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var difyResponse DifyChatCompletionResponse
	responseBody, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.CloseResponseBodyGracefully(resp)
	err = common.Unmarshal(responseBody, &difyResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	fullTextResponse := dto.OpenAITextResponse{
		Id:      difyResponse.ConversationId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Usage:   difyResponse.MetaData.Usage,
	}
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: difyResponse.Answer,
		},
		FinishReason: "stop",
	}
	fullTextResponse.Choices = append(fullTextResponse.Choices, choice)

	var jsonResponse []byte
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		claudeResp := service.ResponseOpenAI2Claude(&fullTextResponse, info)
		jsonResponse, err = common.Marshal(claudeResp)
	default:
		jsonResponse, err = common.Marshal(fullTextResponse)
	}
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.IOCopyBytesGracefully(c, resp, jsonResponse)
	return &difyResponse.MetaData.Usage, nil
}
