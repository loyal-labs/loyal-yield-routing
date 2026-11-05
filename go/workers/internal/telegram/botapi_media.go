package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

const maxDownloadBytes = 20 << 20

func (a *BotAPI) SavePreparedInlineMessage(ctx context.Context, userID int64, result any) (string, error) {
	var saved struct {
		ID string `json:"id"`
	}
	err := a.call(ctx, "savePreparedInlineMessage", map[string]any{
		"user_id":             userID,
		"result":              result,
		"allow_bot_chats":     false,
		"allow_channel_chats": false,
		"allow_group_chats":   false,
		"allow_user_chats":    true,
	}, &saved)
	return saved.ID, err
}

func (a *BotAPI) FirstProfilePhotoFileID(ctx context.Context, userID int64) (string, error) {
	var photos struct {
		Photos [][]struct {
			FileID string `json:"file_id"`
		} `json:"photos"`
	}
	if err := a.call(ctx, "getUserProfilePhotos", map[string]any{"user_id": userID, "limit": 1}, &photos); err != nil {
		return "", err
	}
	if len(photos.Photos) == 0 || len(photos.Photos[0]) == 0 {
		return "", nil
	}
	return photos.Photos[0][0].FileID, nil
}

func (a *BotAPI) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	var file struct {
		FilePath string `json:"file_path"`
	}
	if err := a.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/file/bot"+a.token+"/"+file.FilePath, nil)
	if err != nil {
		return nil, "", fmt.Errorf("telegram file: invalid request")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("telegram file: download failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, "", fmt.Errorf("telegram file: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes))
	if err != nil {
		return nil, "", fmt.Errorf("telegram file: read: %w", err)
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return body, contentType, nil
}
