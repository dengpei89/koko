package handler

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gliderlabs/ssh"

	"github.com/jumpserver/koko/pkg/common"
	"github.com/jumpserver/koko/pkg/i18n"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/model"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/service"
	"github.com/jumpserver/koko/pkg/logger"
	"github.com/jumpserver/koko/pkg/proxy"
	"github.com/jumpserver/koko/pkg/utils"
)

/*
直接连接资产使用的登录名，支持使用以下四种格式：

1. JMS_username@systemUser_username@asset_ip
2. JMS_username#systemUser_username#asset_ip
3. JMS_username@systemUser_uuid@asset_uuid
4. JMS_username#systemUser_uuid#asset_uuid

JMS_username: 			JumpServer 平台上的用户名
systemUser_username：	对应系统用户的用户名
asset_ip: 				对应资产的ip
systemUser_uuid:		对应系统用户的UUID
asset_uuid:				对应资产的UUID


FormatNORMAL: 使用 systemUser_username 和 asset_ip 的登录方式，即1和2的方式

FormatUUID:  使用 systemUser_uuid 和 asset_uuid 的登录方式，即3和4的方式

FormatToken:  使用 JMS-{token} 的方式登陆方式

*/

type FormatType int

const (
	FormatNORMAL FormatType = iota
	FormatUUID
	FormatToken
)

type DirectOpt func(*directOpt)

type directOpt struct {
	targetAsset      string
	targetSystemUser string
	User             *model.User
	terminalConf     *model.TerminalConfig

	formatType FormatType

	tokenInfo *model.ConnectTokenInfo
}

func (d directOpt) IsTokenConnection() bool {
	return d.formatType == FormatToken
}

func DirectTargetAsset(targetAsset string) DirectOpt {
	return func(opts *directOpt) {
		opts.targetAsset = targetAsset
	}
}

func DirectTargetSystemUser(targetSystemUser string) DirectOpt {
	return func(opts *directOpt) {
		opts.targetSystemUser = targetSystemUser
	}
}

func DirectUser(User *model.User) DirectOpt {
	return func(opts *directOpt) {
		opts.User = User
	}
}

func DirectTerminalConf(conf *model.TerminalConfig) DirectOpt {
	return func(opts *directOpt) {
		opts.terminalConf = conf
	}
}

func DirectFormatType(format FormatType) DirectOpt {
	return func(opts *directOpt) {
		opts.formatType = format
	}
}

func DirectConnectToken(tokenInfo *model.ConnectTokenInfo) DirectOpt {
	return func(opts *directOpt) {
		opts.tokenInfo = tokenInfo
	}
}

func selectAssetsByDirectOpt(jmsService *service.JMService, opts *directOpt) ([]model.Asset, error) {
	switch opts.formatType {
	case FormatUUID:
		asset, err := jmsService.GetAssetById(opts.targetAsset)
		if err != nil {
			return nil, err
		}
		return []model.Asset{asset}, nil
	default:
		return jmsService.GetUserPermAssetsByIP(opts.User.ID, opts.targetAsset)
	}
}

func NewDirectHandler(sess ssh.Session, jmsService *service.JMService, optSetters ...DirectOpt) (*DirectHandler, error) {
	opts := &directOpt{}
	for i := range optSetters {
		optSetters[i](opts)
	}
	var (
		selectedAssets []model.Asset
		err            error
	)
	if !opts.IsTokenConnection() {
		selectedAssets, err = selectAssetsByDirectOpt(jmsService, opts)
		if err != nil {
			logger.Errorf("Get direct asset failed: %s", err)
			utils.IgnoreErrWriteString(sess, i18n.T("Core API failed"))
			return nil, err
		}
		if len(selectedAssets) <= 0 {
			msg := fmt.Sprintf(i18n.T("not found matched asset %s"), opts.targetAsset)
			utils.IgnoreErrWriteString(sess, msg+"\r\n")
			return nil, fmt.Errorf("no found matched asset: %s", opts.targetAsset)
		}
	}

	wrapperSess := NewWrapperSession(sess)
	term := utils.NewTerminal(wrapperSess, "Opt> ")
	d := &DirectHandler{
		sess:        sess,
		wrapperSess: wrapperSess,
		opts:        opts,
		jmsService:  jmsService,
		assets:      selectedAssets,
		term:        term,
	}
	return d, err

}

type DirectHandler struct {
	term        *utils.Terminal
	sess        ssh.Session
	wrapperSess *WrapperSession
	opts        *directOpt
	jmsService  *service.JMService

	IsPtyStatus bool

	assets []model.Asset

	selectedSystemUser *model.SystemUser
}

func (d *DirectHandler) Dispatch() {
	_, winChan, _ := d.sess.Pty()
	go d.WatchWinSizeChange(winChan)
	if d.opts.IsTokenConnection() {
		d.LoginConnectToken()
		return
	}
	d.LoginAsset()
}

func (d *DirectHandler) WatchWinSizeChange(winChan <-chan ssh.Window) {
	defer logger.Infof("Request %s: Windows change watch close", d.wrapperSess.Uuid)
	for {
		select {
		case <-d.sess.Context().Done():
			return
		case win, ok := <-winChan:
			if !ok {
				return
			}
			d.wrapperSess.SetWin(win)
			logger.Debugf("Term window size change: %d*%d", win.Height, win.Width)
			_ = d.term.SetSize(win.Width, win.Height)
		}
	}
}

func (d *DirectHandler) LoginAsset() {
	switch len(d.assets) {
	case 1:
		d.Proxy(d.assets[0])
	default:
		for {
			d.displayAssets(d.assets)
			num, err := d.term.ReadLine()
			if err != nil {
				logger.Error(err)
				return
			}
			if indexNum, err2 := strconv.Atoi(num); err2 == nil && len(d.assets) > 0 {
				if indexNum > 0 && indexNum <= len(d.assets) {
					d.Proxy(d.assets[indexNum-1])
					return
				}
			}
			switch num {
			case "q", "quit", "exit":
				logger.Infof("User %s enter %s to exit ", d.opts.User, num)
				return
			}
		}
	}
}

func (d *DirectHandler) selectSystemUsers(systemUsers []model.SystemUser) (model.SystemUser, bool) {
	length := len(systemUsers)
	switch length {
	case 0:
		warningInfo := i18n.T("No system user found.")
		_, _ = io.WriteString(d.sess, warningInfo+"\n\r")
		return model.SystemUser{}, false
	case 1:
		return systemUsers[0], true
	default:
	}
	displaySystemUsers := selectHighestPrioritySystemUsers(systemUsers)
	if len(displaySystemUsers) == 1 {
		return displaySystemUsers[0], true
	}

	idLabel := i18n.T("ID")
	nameLabel := i18n.T("Name")
	usernameLabel := i18n.T("Username")

	labels := []string{idLabel, nameLabel, usernameLabel}
	fields := []string{"ID", "Name", "Username"}

	data := make([]map[string]string, len(displaySystemUsers))
	for i, j := range displaySystemUsers {
		row := make(map[string]string)
		row["ID"] = strconv.Itoa(i + 1)
		row["Name"] = j.Name
		row["Username"] = j.Username
		data[i] = row
	}
	pty, _, _ := d.sess.Pty()
	table := common.WrapperTable{
		Fields: fields,
		Labels: labels,
		FieldsSize: map[string][3]int{
			"ID":       {0, 0, 5},
			"Name":     {0, 8, 0},
			"Username": {0, 10, 0},
		},
		Data:        data,
		TotalSize:   pty.Window.Width,
		TruncPolicy: common.TruncMiddle,
	}
	table.Initial()

	d.term.SetPrompt("ID> ")
	selectTip := i18n.T("Tips: Enter system user ID and directly login")
	backTip := i18n.T("Back: B/b")
	for {
		utils.IgnoreErrWriteString(d.term, table.Display())
		utils.IgnoreErrWriteString(d.term, utils.WrapperString(selectTip, utils.Green))
		utils.IgnoreErrWriteString(d.term, utils.CharNewLine)
		utils.IgnoreErrWriteString(d.term, utils.WrapperString(backTip, utils.Green))
		utils.IgnoreErrWriteString(d.term, utils.CharNewLine)
		line, err := d.term.ReadLine()
		if err != nil {
			return model.SystemUser{}, false
		}
		line = strings.TrimSpace(line)
		switch strings.ToLower(line) {
		case "q", "b", "quit", "exit", "back":
			return model.SystemUser{}, false
		}
		if num, err := strconv.Atoi(line); err == nil {
			if num > 0 && num <= len(displaySystemUsers) {
				return displaySystemUsers[num-1], true
			}
		}
	}
}

func (d *DirectHandler) displayAssets(assets []model.Asset) {
	assetListSortBy := d.opts.terminalConf.AssetListSortBy
	model.AssetList(assets).SortBy(assetListSortBy)

	term := d.term

	idLabel := i18n.T("ID")
	hostLabel := i18n.T("Hostname")
	ipLabel := i18n.T("IP")
	commentLabel := i18n.T("Comment")

	Labels := []string{idLabel, hostLabel, ipLabel, commentLabel}
	fields := []string{"ID", "Hostname", "IP", "Comment"}
	data := make([]map[string]string, len(assets))
	for i := range assets {
		row := make(map[string]string)
		row["ID"] = strconv.Itoa(i + 1)
		row["Hostname"] = assets[i].Hostname
		row["IP"] = assets[i].IP
		row["Comment"] = joinMultiLineString(assets[i].Comment)
		data[i] = row
	}
	w, _ := d.term.GetSize()

	table := common.WrapperTable{
		Fields: fields,
		Labels: Labels,
		FieldsSize: map[string][3]int{
			"ID":       {0, 0, 5},
			"Hostname": {0, 40, 0},
			"IP":       {0, 15, 40},
			"Comment":  {0, 0, 0},
		},
		Data:        data,
		TotalSize:   w,
		TruncPolicy: common.TruncMiddle,
	}
	table.Initial()
	loginTip := i18n.T("select one asset to login")

	_, _ = term.Write([]byte(utils.CharClear))
	_, _ = term.Write([]byte(table.Display()))
	utils.IgnoreErrWriteString(term, utils.WrapperString(loginTip, utils.Green))
	utils.IgnoreErrWriteString(term, utils.CharNewLine)
	utils.IgnoreErrWriteString(term, utils.WrapperString(d.opts.targetAsset, utils.Green))
	utils.IgnoreErrWriteString(term, utils.CharNewLine)
}

func (d *DirectHandler) Proxy(asset model.Asset) {
	matched := d.getMatchedSystemUsers(asset)
	if len(matched) == 0 {
		msg := fmt.Sprintf(i18n.T("not found matched username %s"), d.opts.targetSystemUser)
		utils.IgnoreErrWriteString(d.term, msg+"\r\n")
		logger.Errorf("Get systemUser failed: %s", msg)
		return
	}
	selectSys, ok := d.selectSystemUsers(matched)
	if !ok {
		logger.Info("Do not select system user")
		return
	}
	d.selectedSystemUser = &selectSys
	srv, err := proxy.NewServer(d.wrapperSess,
		d.jmsService,
		proxy.ConnectProtocolType(d.selectedSystemUser.Protocol),
		proxy.ConnectUser(d.opts.User),
		proxy.ConnectAsset(&asset),
		proxy.ConnectSystemUser(d.selectedSystemUser),
	)
	if err != nil {
		logger.Error(err)
		return
	}
	srv.Proxy()
	logger.Infof("Request %s: asset %s proxy end", d.wrapperSess.Uuid, asset.Hostname)

}

func (d *DirectHandler) getMatchedSystemUsers(asset model.Asset) []model.SystemUser {
	switch d.opts.formatType {
	case FormatUUID:
		systemUser, err := d.jmsService.GetSystemUserById(d.opts.targetSystemUser)
		if err != nil {
			logger.Errorf("Get systemUser failed: %s", err)
			utils.IgnoreErrWriteString(d.term, i18n.T("Core API failed"))
			return nil
		}
		return []model.SystemUser{systemUser}
	default:
		systemUsers, err := d.jmsService.GetSystemUsersByUserIdAndAssetId(d.opts.User.ID, asset.ID)
		if err != nil {
			logger.Errorf("Get systemUser failed: %s", err)
			utils.IgnoreErrWriteString(d.term, i18n.T("Core API failed"))
			return nil
		}
		matched := make([]model.SystemUser, 0, len(systemUsers))
		for i := range systemUsers {
			compareUsername := systemUsers[i].Username

			if systemUsers[i].UsernameSameWithUser {
				// 此为动态系统用户，系统用户名和登录用户名相同
				compareUsername = d.opts.User.Username
			}
			if compareUsername == d.opts.targetSystemUser {
				matched = append(matched, systemUsers[i])
			}
		}
		return matched
	}
}
