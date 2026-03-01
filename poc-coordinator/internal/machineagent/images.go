package machineagent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validUserID = regexp.MustCompile(`^[a-zA-Z0-9-]{3,32}$`)

func validateUserID(userID string) error {
	if !validUserID.MatchString(userID) {
		return fmt.Errorf("invalid user_id %q: must be 3-32 alphanumeric/hyphen chars", userID)
	}
	return nil
}

type ImageCreateRequest struct {
	ImageSizeMB int `json:"image_size_mb"`
}

type ImageCreateResponse struct {
	LoopDevice     string `json:"loop_device"`
	ImagePath      string `json:"image_path"`
	AlreadyExisted bool   `json:"already_existed"`
}

func (a *Agent) CreateImage(userID string, sizeMB int) (*ImageCreateResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	imgPath := a.imagePath(userID)

	// Check if image already exists
	if _, err := os.Stat(imgPath); err == nil {
		// Image exists — check for loop device
		loop := a.findLoopDevice(imgPath)
		if loop != "" {
			slog.Info("Image already exists with loop device", "component", "images", "user", userID, "loop", loop)
			return &ImageCreateResponse{
				LoopDevice:     loop,
				ImagePath:      imgPath,
				AlreadyExisted: true,
			}, nil
		}
		// Image exists but no loop device — attach one
		loop, err := a.attachLoop(imgPath)
		if err != nil {
			return nil, fmt.Errorf("attach loop device: %w", err)
		}
		u := a.getUser(userID)
		if u == nil {
			u = &UserResources{}
		}
		u.ImagePath = imgPath
		u.LoopDevice = loop
		a.setUser(userID, u)
		return &ImageCreateResponse{
			LoopDevice:     loop,
			ImagePath:      imgPath,
			AlreadyExisted: true,
		}, nil
	}

	// Create sparse image
	dir := filepath.Dir(imgPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir images dir: %w", err)
	}

	result, err := runCmd("truncate", "-s", fmt.Sprintf("%dM", sizeMB), imgPath)
	if err != nil {
		return nil, cmdError("truncate failed", cmdString("truncate", "-s", fmt.Sprintf("%dM", sizeMB), imgPath), result)
	}

	// Attach loop device
	loop, err := a.attachLoop(imgPath)
	if err != nil {
		return nil, fmt.Errorf("attach loop device: %w", err)
	}

	u := a.getUser(userID)
	if u == nil {
		u = &UserResources{}
	}
	u.ImagePath = imgPath
	u.LoopDevice = loop
	a.setUser(userID, u)

	slog.Info("Image created", "component", "images", "user", userID, "loop_device", loop)
	return &ImageCreateResponse{
		LoopDevice: loop,
		ImagePath:  imgPath,
	}, nil
}

func (a *Agent) findLoopDevice(imgPath string) string {
	result, err := runCmd("losetup", "-a")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.Contains(line, imgPath) {
			m := loopRe.FindStringSubmatch(line)
			if m != nil {
				return m[1]
			}
		}
	}
	return ""
}

func (a *Agent) attachLoop(imgPath string) (string, error) {
	result, err := runCmd("losetup", "-f", "--show", imgPath)
	if err != nil {
		return "", cmdError("losetup failed", cmdString("losetup", "-f", "--show", imgPath), result)
	}
	return strings.TrimSpace(result.Stdout), nil
}
