// Copyright 2024, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/skeema/knownhosts"
	"github.com/wavetermdev/waveterm/pkg/trimquotes"
	"github.com/wavetermdev/waveterm/pkg/userinput"
	"github.com/wavetermdev/waveterm/pkg/util/shellutil"
	"github.com/wavetermdev/waveterm/pkg/wavebase"
	"github.com/wavetermdev/waveterm/pkg/wconfig"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

const SshProxyJumpMaxDepth = 10

var waveSshConfigUserSettingsInternal *ssh_config.UserSettings
var configUserSettingsOnce = &sync.Once{}

func WaveSshConfigUserSettings() *ssh_config.UserSettings {
	configUserSettingsOnce.Do(func() {
		waveSshConfigUserSettingsInternal = ssh_config.DefaultUserSettings
		waveSshConfigUserSettingsInternal.IgnoreMatchDirective = true
	})
	return waveSshConfigUserSettingsInternal
}

type UserInputCancelError struct {
	Err error
}

type HostKeyAlgorithms = func(hostWithPort string) (algos []string)

func (uice UserInputCancelError) Error() string {
	return uice.Err.Error()
}

type ConnectionDebugInfo struct {
	CurrentClient *ssh.Client
	NextOpts      *SSHOpts
	JumpNum       int32
}

type ConnectionError struct {
	*ConnectionDebugInfo
	Err error
}

func (ce ConnectionError) Error() string {
	if ce.CurrentClient == nil {
		return fmt.Sprintf("Connecting to %+#v, Error: %v", ce.NextOpts, ce.Err)
	}
	return fmt.Sprintf("Connecting from %v to %+#v (jump number %d), Error: %v", ce.CurrentClient, ce.NextOpts, ce.JumpNum, ce.Err)
}

// This exists to trick the ssh library into continuing to try
// different public keys even when the current key cannot be
// properly parsed
func createDummySigner() ([]ssh.Signer, error) {
	dummyKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	dummySigner, err := ssh.NewSignerFromKey(dummyKey)
	if err != nil {
		return nil, err
	}
	return []ssh.Signer{dummySigner}, nil

}

// This is a workaround to only process one identity file at a time,
// even if they have passphrases. It must be combined with retryable
// authentication to work properly
//
// Despite returning an array of signers, we only ever provide one since
// it allows proper user interaction in between attempts
//
// A significant number of errors end up returning dummy values as if
// they were successes. An error in this function prevents any other
// keys from being attempted. But if there's an error because of a dummy
// file, the library can still try again with a new key.
func createPublicKeyCallback(connCtx context.Context, sshKeywords *wshrpc.ConnKeywords, authSockSignersExt []ssh.Signer, agentClient agent.ExtendedAgent, debugInfo *ConnectionDebugInfo) func() ([]ssh.Signer, error) {
	var identityFiles []string
	existingKeys := make(map[string][]byte)

	// checking the file early prevents us from needing to send a
	// dummy signer if there's a problem with the signer
	for _, identityFile := range sshKeywords.SshIdentityFile {
		filePath, err := wavebase.ExpandHomeDir(identityFile)
		if err != nil {
			continue
		}
		privateKey, err := os.ReadFile(filePath)
		if err != nil {
			// skip this key and try with the next
			continue
		}
		existingKeys[identityFile] = privateKey
		identityFiles = append(identityFiles, identityFile)
	}
	// require pointer to modify list in closure
	identityFilesPtr := &identityFiles

	var authSockSigners []ssh.Signer
	authSockSigners = append(authSockSigners, authSockSignersExt...)
	authSockSignersPtr := &authSockSigners

	return func() ([]ssh.Signer, error) {
		// try auth sock
		if len(*authSockSignersPtr) != 0 {
			authSockSigner := (*authSockSignersPtr)[0]
			*authSockSignersPtr = (*authSockSignersPtr)[1:]
			return []ssh.Signer{authSockSigner}, nil
		}

		if len(*identityFilesPtr) == 0 {
			return nil, ConnectionError{ConnectionDebugInfo: debugInfo, Err: fmt.Errorf("no identity files remaining")}
		}
		identityFile := (*identityFilesPtr)[0]
		*identityFilesPtr = (*identityFilesPtr)[1:]
		privateKey, ok := existingKeys[identityFile]
		if !ok {
			log.Printf("error with existingKeys, this should never happen")
			// skip this key and try with the next
			return createDummySigner()
		}

		unencryptedPrivateKey, err := ssh.ParseRawPrivateKey(privateKey)
		if err == nil {
			signer, err := ssh.NewSignerFromKey(unencryptedPrivateKey)
			if err == nil {
				if sshKeywords.SshAddKeysToAgent && agentClient != nil {
					agentClient.Add(agent.AddedKey{
						PrivateKey: unencryptedPrivateKey,
					})
				}
				return []ssh.Signer{signer}, nil
			}
		}
		if _, ok := err.(*ssh.PassphraseMissingError); !ok {
			// skip this key and try with the next
			return createDummySigner()
		}

		// batch mode deactivates user input
		if sshKeywords.SshBatchMode {
			// skip this key and try with the next
			return createDummySigner()
		}

		request := &userinput.UserInputRequest{
			ResponseType: "text",
			QueryText:    fmt.Sprintf("Enter passphrase for the SSH key: %s", identityFile),
			Title:        "Publickey Auth + Passphrase",
		}
		ctx, cancelFn := context.WithTimeout(connCtx, 60*time.Second)
		defer cancelFn()
		response, err := userinput.GetUserInput(ctx, request)
		if err != nil {
			// this is an error where we actually do want to stop
			// trying keys

			return nil, ConnectionError{ConnectionDebugInfo: debugInfo, Err: UserInputCancelError{Err: err}}
		}
		unencryptedPrivateKey, err = ssh.ParseRawPrivateKeyWithPassphrase(privateKey, []byte([]byte(response.Text)))
		if err != nil {
			// skip this key and try with the next
			return createDummySigner()
		}
		signer, err := ssh.NewSignerFromKey(unencryptedPrivateKey)
		if err != nil {
			// skip this key and try with the next
			return createDummySigner()
		}
		if sshKeywords.SshAddKeysToAgent && agentClient != nil {
			agentClient.Add(agent.AddedKey{
				PrivateKey: unencryptedPrivateKey,
			})
		}
		return []ssh.Signer{signer}, nil
	}
}

func createInteractivePasswordCallbackPrompt(connCtx context.Context, remoteDisplayName string, debugInfo *ConnectionDebugInfo) func() (secret string, err error) {
	return func() (secret string, err error) {
		ctx, cancelFn := context.WithTimeout(connCtx, 60*time.Second)
		defer cancelFn()
		queryText := fmt.Sprintf(
			"Password Authentication requested from connection  \n"+
				"%s\n\n"+
				"Password:", remoteDisplayName)
		request := &userinput.UserInputRequest{
			ResponseType: "text",
			QueryText:    queryText,
			Markdown:     true,
			Title:        "Password Authentication",
		}
		response, err := userinput.GetUserInput(ctx, request)
		if err != nil {
			return "", ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
		}
		return response.Text, nil
	}
}

func createInteractiveKbdInteractiveChallenge(connCtx context.Context, remoteName string, debugInfo *ConnectionDebugInfo) func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
	return func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
		if len(questions) != len(echos) {
			return nil, fmt.Errorf("bad response from server: questions has len %d, echos has len %d", len(questions), len(echos))
		}
		for i, question := range questions {
			echo := echos[i]
			answer, err := promptChallengeQuestion(connCtx, question, echo, remoteName)
			if err != nil {
				return nil, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
			}
			answers = append(answers, answer)
		}
		return answers, nil
	}
}

func promptChallengeQuestion(connCtx context.Context, question string, echo bool, remoteName string) (answer string, err error) {
	// limited to 15 seconds for some reason. this should be investigated more
	// in the future
	ctx, cancelFn := context.WithTimeout(connCtx, 60*time.Second)
	defer cancelFn()
	queryText := fmt.Sprintf(
		"Keyboard Interactive Authentication requested from connection  \n"+
			"%s\n\n"+
			"%s", remoteName, question)
	request := &userinput.UserInputRequest{
		ResponseType: "text",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Keyboard Interactive Authentication",
		PublicText:   echo,
	}
	response, err := userinput.GetUserInput(ctx, request)
	if err != nil {
		return "", err
	}
	return response.Text, nil
}

func openKnownHostsForEdit(knownHostsFilename string) (*os.File, error) {
	path, _ := filepath.Split(knownHostsFilename)
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(knownHostsFilename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
}

func writeToKnownHosts(knownHostsFile string, newLine string, getUserVerification func() (*userinput.UserInputResponse, error)) error {
	if getUserVerification == nil {
		getUserVerification = func() (*userinput.UserInputResponse, error) {
			return &userinput.UserInputResponse{
				Type:    "confirm",
				Confirm: true,
			}, nil
		}
	}

	path, _ := filepath.Split(knownHostsFile)
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(knownHostsFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	// do not close writeable files with defer

	// this file works, so let's ask the user for permission
	response, err := getUserVerification()
	if err != nil {
		f.Close()
		return UserInputCancelError{Err: err}
	}
	if !response.Confirm {
		f.Close()
		return UserInputCancelError{Err: fmt.Errorf("canceled by the user")}
	}

	_, err = f.WriteString(newLine + "\n")
	if err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func createUnknownKeyVerifier(knownHostsFile string, hostname string, remote string, key ssh.PublicKey) func() (*userinput.UserInputResponse, error) {
	base64Key := base64.StdEncoding.EncodeToString(key.Marshal())
	queryText := fmt.Sprintf(
		"The authenticity of host '%s (%s)' can't be established "+
			"as it **does not exist in any checked known_hosts files**. "+
			"The host you are attempting to connect to provides this %s key:  \n"+
			"%s.\n\n"+
			"**Would you like to continue connecting?** If so, the key will be permanently "+
			"added to the file %s "+
			"to protect from future man-in-the-middle attacks.", hostname, remote, key.Type(), base64Key, knownHostsFile)
	request := &userinput.UserInputRequest{
		ResponseType: "confirm",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Known Hosts Key Missing",
	}
	return func() (*userinput.UserInputResponse, error) {
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		resp, err := userinput.GetUserInput(ctx, request)
		if err != nil {
			return nil, err
		}
		if !resp.Confirm {
			return nil, fmt.Errorf("user selected no")
		}
		return resp, nil
	}
}

func createMissingKnownHostsVerifier(knownHostsFile string, hostname string, remote string, key ssh.PublicKey) func() (*userinput.UserInputResponse, error) {
	base64Key := base64.StdEncoding.EncodeToString(key.Marshal())
	queryText := fmt.Sprintf(
		"The authenticity of host '%s (%s)' can't be established "+
			"as **no known_hosts files could be found**. "+
			"The host you are attempting to connect to provides this %s key:  \n"+
			"%s.\n\n"+
			"**Would you like to continue connecting?** If so:  \n"+
			"- %s will be created  \n"+
			"- the key will be added to %s\n\n"+
			"This will protect from future man-in-the-middle attacks.", hostname, remote, key.Type(), base64Key, knownHostsFile, knownHostsFile)
	request := &userinput.UserInputRequest{
		ResponseType: "confirm",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Known Hosts File Missing",
	}
	return func() (*userinput.UserInputResponse, error) {
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		resp, err := userinput.GetUserInput(ctx, request)
		if err != nil {
			return nil, err
		}
		if !resp.Confirm {
			return nil, fmt.Errorf("user selected no")
		}
		return resp, nil
	}
}

func lineContainsMatch(line []byte, matches [][]byte) bool {
	for _, match := range matches {
		if bytes.Contains(line, match) {
			return true
		}
	}
	return false
}

func createHostKeyCallback(sshKeywords *wshrpc.ConnKeywords) (ssh.HostKeyCallback, HostKeyAlgorithms, error) {
	globalKnownHostsFiles := sshKeywords.SshGlobalKnownHostsFile
	userKnownHostsFiles := sshKeywords.SshUserKnownHostsFile

	osUser, err := user.Current()
	if err != nil {
		return nil, nil, err
	}
	var unexpandedKnownHostsFiles []string
	if osUser.Username == "root" {
		unexpandedKnownHostsFiles = globalKnownHostsFiles
	} else {
		unexpandedKnownHostsFiles = append(userKnownHostsFiles, globalKnownHostsFiles...)
	}

	var knownHostsFiles []string
	for _, filename := range unexpandedKnownHostsFiles {
		filePath, err := wavebase.ExpandHomeDir(filename)
		if err != nil {
			continue
		}
		knownHostsFiles = append(knownHostsFiles, filePath)
	}

	// there are no good known hosts files
	if len(knownHostsFiles) == 0 {
		return nil, nil, fmt.Errorf("no known_hosts files provided by ssh. defaults are overridden")
	}

	var unreadableFiles []string

	// the library we use isn't very forgiving about files that are formatted
	// incorrectly. if a problem file is found, it is removed from our list
	// and we try again
	var basicCallback ssh.HostKeyCallback
	var hostKeyAlgorithms HostKeyAlgorithms
	for basicCallback == nil && len(knownHostsFiles) > 0 {
		keyDb, err := knownhosts.NewDB(knownHostsFiles...)
		if serr, ok := err.(*os.PathError); ok {
			badFile := serr.Path
			unreadableFiles = append(unreadableFiles, badFile)
			var okFiles []string
			for _, filename := range knownHostsFiles {
				if filename != badFile {
					okFiles = append(okFiles, filename)
				}
			}
			if len(okFiles) >= len(knownHostsFiles) {
				return nil, nil, fmt.Errorf("problem file (%s) doesn't exist. this should not be possible", badFile)
			}
			knownHostsFiles = okFiles
		} else if err != nil {
			// TODO handle obscure problems if possible
			return nil, nil, fmt.Errorf("known_hosts formatting error: %+v", err)
		} else {
			basicCallback = keyDb.HostKeyCallback()
			hostKeyAlgorithms = keyDb.HostKeyAlgorithms
		}
	}

	if basicCallback == nil {
		basicCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return &xknownhosts.KeyError{}
		}
		// need to return nil here to avoid null pointer from attempting to call
		// the one provided by the db if nothing was found
		hostKeyAlgorithms = func(hostWithPort string) (algos []string) {
			return nil
		}
	}

	waveHostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := basicCallback(hostname, remote, key)
		if err == nil {
			// success
			return nil
		} else if _, ok := err.(*xknownhosts.RevokedError); ok {
			// revoked credentials are refused outright
			return err
		} else if _, ok := err.(*xknownhosts.KeyError); !ok {
			// this is an unknown error (note the !ok is opposite of usual)
			return err
		}
		serr, _ := err.(*xknownhosts.KeyError)
		if len(serr.Want) == 0 {
			// the key was not found

			// try to write to a file that could be read
			err := fmt.Errorf("placeholder, should not be returned") // a null value here can cause problems with empty slice
			for _, filename := range knownHostsFiles {
				newLine := xknownhosts.Line([]string{xknownhosts.Normalize(hostname)}, key)
				getUserVerification := createUnknownKeyVerifier(filename, hostname, remote.String(), key)
				err = writeToKnownHosts(filename, newLine, getUserVerification)
				if err == nil {
					break
				}
				if serr, ok := err.(UserInputCancelError); ok {
					return serr
				}
			}

			// try to write to a file that could not be read (file likely doesn't exist)
			// should catch cases where there is no known_hosts file
			if err != nil {
				for _, filename := range unreadableFiles {
					newLine := xknownhosts.Line([]string{xknownhosts.Normalize(hostname)}, key)
					getUserVerification := createMissingKnownHostsVerifier(filename, hostname, remote.String(), key)
					err = writeToKnownHosts(filename, newLine, getUserVerification)
					if err == nil {
						knownHostsFiles = []string{filename}
						break
					}
					if serr, ok := err.(UserInputCancelError); ok {
						return serr
					}
				}
			}
			if err != nil {
				return fmt.Errorf("unable to create new knownhost key: %e", err)
			}
		} else {
			// the key changed
			correctKeyFingerprint := base64.StdEncoding.EncodeToString(key.Marshal())
			var bulletListKnownHosts []string
			for _, knownHostName := range knownHostsFiles {
				withBulletPoint := "- " + knownHostName
				bulletListKnownHosts = append(bulletListKnownHosts, withBulletPoint)
			}
			var offendingKeysFmt []string
			for _, badKey := range serr.Want {
				formattedKey := "- " + base64.StdEncoding.EncodeToString(badKey.Key.Marshal())
				offendingKeysFmt = append(offendingKeysFmt, formattedKey)
			}
			// todo
			errorMsg := fmt.Sprintf("**WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!**\n\n"+
				"If this is not expected, it is possible that someone could be trying to "+
				"eavesdrop on you via a man-in-the-middle attack. "+
				"Alternatively, the host you are connecting to may have changed its key. "+
				"The %s key sent by the remote hist has the fingerprint:  \n"+
				"%s\n\n"+
				"If you are sure this is correct, please update your known_hosts files to "+
				"remove the lines with the offending before trying to connect again.  \n"+
				"**Known Hosts Files**  \n"+
				"%s\n\n"+
				"**Offending Keys**  \n"+
				"%s", key.Type(), correctKeyFingerprint, strings.Join(bulletListKnownHosts, "  \n"), strings.Join(offendingKeysFmt, "  \n"))

			log.Print(errorMsg)
			//update := scbus.MakeUpdatePacket()
			// create update into alert message

			//send update via bus?
			return fmt.Errorf("remote host identification has changed")
		}

		updatedCallback, err := xknownhosts.New(knownHostsFiles...)
		if err != nil {
			return err
		}
		// try one final time
		return updatedCallback(hostname, remote, key)
	}

	return waveHostKeyCallback, hostKeyAlgorithms, nil
}

func createClientConfig(connCtx context.Context, sshKeywords *wshrpc.ConnKeywords, debugInfo *ConnectionDebugInfo) (*ssh.ClientConfig, error) {
	remoteName := sshKeywords.SshUser + "@" + xknownhosts.Normalize(sshKeywords.SshHostName+":"+sshKeywords.SshPort)

	var authSockSigners []ssh.Signer
	var agentClient agent.ExtendedAgent
	conn, err := net.Dial("unix", sshKeywords.SshIdentityAgent)
	if err != nil {
		log.Printf("Failed to open Identity Agent Socket: %v", err)
	} else {
		agentClient = agent.NewClient(conn)
		authSockSigners, _ = agentClient.Signers()
	}

	publicKeyCallback := ssh.PublicKeysCallback(createPublicKeyCallback(connCtx, sshKeywords, authSockSigners, agentClient, debugInfo))
	keyboardInteractive := ssh.KeyboardInteractive(createInteractiveKbdInteractiveChallenge(connCtx, remoteName, debugInfo))
	passwordCallback := ssh.PasswordCallback(createInteractivePasswordCallbackPrompt(connCtx, remoteName, debugInfo))

	// exclude gssapi-with-mic and hostbased until implemented
	authMethodMap := map[string]ssh.AuthMethod{
		"publickey":            ssh.RetryableAuthMethod(publicKeyCallback, len(sshKeywords.SshIdentityFile)+len(authSockSigners)),
		"keyboard-interactive": ssh.RetryableAuthMethod(keyboardInteractive, 1),
		"password":             ssh.RetryableAuthMethod(passwordCallback, 1),
	}

	// note: batch mode turns off interactive input
	authMethodActiveMap := map[string]bool{
		"publickey":            sshKeywords.SshPubkeyAuthentication,
		"keyboard-interactive": sshKeywords.SshKbdInteractiveAuthentication && !sshKeywords.SshBatchMode,
		"password":             sshKeywords.SshPasswordAuthentication && !sshKeywords.SshBatchMode,
	}

	var authMethods []ssh.AuthMethod
	for _, authMethodName := range sshKeywords.SshPreferredAuthentications {
		authMethodActive, ok := authMethodActiveMap[authMethodName]
		if !ok || !authMethodActive {
			continue
		}
		authMethod, ok := authMethodMap[authMethodName]
		if !ok {
			continue
		}
		authMethods = append(authMethods, authMethod)
	}

	hostKeyCallback, hostKeyAlgorithms, err := createHostKeyCallback(sshKeywords)
	if err != nil {
		return nil, err
	}

	networkAddr := sshKeywords.SshHostName + ":" + sshKeywords.SshPort
	return &ssh.ClientConfig{
		User:              sshKeywords.SshUser,
		Auth:              authMethods,
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgorithms(networkAddr),
	}, nil
}

func connectInternal(ctx context.Context, networkAddr string, clientConfig *ssh.ClientConfig, currentClient *ssh.Client) (*ssh.Client, error) {
	var clientConn net.Conn
	var err error
	if currentClient == nil {
		d := net.Dialer{Timeout: clientConfig.Timeout}
		clientConn, err = d.DialContext(ctx, "tcp", networkAddr)
		if err != nil {
			return nil, err
		}
	} else {
		clientConn, err = currentClient.DialContext(ctx, "tcp", networkAddr)
		if err != nil {
			return nil, err
		}
	}
	c, chans, reqs, err := ssh.NewClientConn(clientConn, networkAddr, clientConfig)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func ConnectToClient(connCtx context.Context, opts *SSHOpts, currentClient *ssh.Client, jumpNum int32, connFlags *wshrpc.ConnKeywords) (*ssh.Client, int32, error) {
	debugInfo := &ConnectionDebugInfo{
		CurrentClient: currentClient,
		NextOpts:      opts,
		JumpNum:       jumpNum,
	}
	if jumpNum > SshProxyJumpMaxDepth {
		return nil, jumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: fmt.Errorf("ProxyJump %d exceeds Wave's max depth of %d", jumpNum, SshProxyJumpMaxDepth)}
	}
	// todo print final warning if logging gets turned off
	sshConfigKeywords, err := findSshConfigKeywords(opts.SSHHost)
	if err != nil {
		return nil, debugInfo.JumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
	}

	connFlags.SshUser = opts.SSHUser
	connFlags.SshHostName = opts.SSHHost
	connFlags.SshPort = fmt.Sprintf("%d", opts.SSHPort)

	rawName := opts.String()
	savedKeywords, ok := wconfig.ReadFullConfig().Connections[rawName]
	if !ok {
		savedKeywords = wshrpc.ConnKeywords{}
	}

	sshKeywords, err := combineSshKeywords(connFlags, sshConfigKeywords, &savedKeywords)
	if err != nil {
		return nil, debugInfo.JumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
	}

	for _, proxyName := range sshKeywords.SshProxyJump {
		proxyOpts, err := ParseOpts(proxyName)
		if err != nil {
			return nil, debugInfo.JumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
		}

		// ensure no overflow (this will likely never happen)
		if jumpNum < math.MaxInt32 {
			jumpNum += 1
		}

		// do not apply supplied keywords to proxies - ssh config must be used for that
		debugInfo.CurrentClient, jumpNum, err = ConnectToClient(connCtx, proxyOpts, debugInfo.CurrentClient, jumpNum, &wshrpc.ConnKeywords{})
		if err != nil {
			// do not add a context on a recursive call
			// (this can cause a recursive nested context that's arbitrarily deep)
			return nil, jumpNum, err
		}
	}
	clientConfig, err := createClientConfig(connCtx, sshKeywords, debugInfo)
	if err != nil {
		return nil, debugInfo.JumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
	}
	networkAddr := sshKeywords.SshHostName + ":" + sshKeywords.SshPort
	client, err := connectInternal(connCtx, networkAddr, clientConfig, debugInfo.CurrentClient)
	if err != nil {
		return client, debugInfo.JumpNum, ConnectionError{ConnectionDebugInfo: debugInfo, Err: err}
	}
	return client, debugInfo.JumpNum, nil
}

func combineSshKeywords(userProvidedOpts *wshrpc.ConnKeywords, configKeywords *wshrpc.ConnKeywords, savedKeywords *wshrpc.ConnKeywords) (*wshrpc.ConnKeywords, error) {
	sshKeywords := &wshrpc.ConnKeywords{}

	if userProvidedOpts.SshUser != "" {
		sshKeywords.SshUser = userProvidedOpts.SshUser
	} else if configKeywords.SshUser != "" {
		sshKeywords.SshUser = configKeywords.SshUser
	} else {
		user, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("failed to get user for ssh: %+v", err)
		}
		sshKeywords.SshUser = user.Username
	}

	// we have to check the host value because of the weird way
	// we store the pattern as the hostname for imported remotes
	if configKeywords.SshHostName != "" {
		sshKeywords.SshHostName = configKeywords.SshHostName
	} else {
		sshKeywords.SshHostName = userProvidedOpts.SshHostName
	}

	if userProvidedOpts.SshPort != "0" && userProvidedOpts.SshPort != "22" {
		sshKeywords.SshPort = userProvidedOpts.SshPort
	} else if configKeywords.SshPort != "" && configKeywords.SshPort != "22" {
		sshKeywords.SshPort = configKeywords.SshPort
	} else {
		sshKeywords.SshPort = "22"
	}

	// use internal config ones
	if savedKeywords != nil {
		sshKeywords.SshIdentityFile = append(sshKeywords.SshIdentityFile, savedKeywords.SshIdentityFile...)
	}

	sshKeywords.SshIdentityFile = append(sshKeywords.SshIdentityFile, userProvidedOpts.SshIdentityFile...)
	sshKeywords.SshIdentityFile = append(sshKeywords.SshIdentityFile, configKeywords.SshIdentityFile...)

	// these are not officially supported in the waveterm frontend but can be configured
	// in ssh config files
	sshKeywords.SshBatchMode = configKeywords.SshBatchMode
	sshKeywords.SshPubkeyAuthentication = configKeywords.SshPubkeyAuthentication
	sshKeywords.SshPasswordAuthentication = configKeywords.SshPasswordAuthentication
	sshKeywords.SshKbdInteractiveAuthentication = configKeywords.SshKbdInteractiveAuthentication
	sshKeywords.SshPreferredAuthentications = configKeywords.SshPreferredAuthentications
	sshKeywords.SshAddKeysToAgent = configKeywords.SshAddKeysToAgent
	sshKeywords.SshIdentityAgent = configKeywords.SshIdentityAgent
	sshKeywords.SshProxyJump = configKeywords.SshProxyJump
	sshKeywords.SshUserKnownHostsFile = configKeywords.SshUserKnownHostsFile
	sshKeywords.SshGlobalKnownHostsFile = configKeywords.SshGlobalKnownHostsFile

	return sshKeywords, nil
}

// note that a `var == "yes"` will default to false
// but `var != "no"` will default to true
// when given unexpected strings
func findSshConfigKeywords(hostPattern string) (*wshrpc.ConnKeywords, error) {
	WaveSshConfigUserSettings().ReloadConfigs()
	sshKeywords := &wshrpc.ConnKeywords{}
	var err error
	//config := wconfig.ReadFullConfig()

	userRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "User")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshUser = trimquotes.TryTrimQuotes(userRaw)

	hostNameRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "HostName")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshHostName = trimquotes.TryTrimQuotes(hostNameRaw)

	portRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "Port")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshPort = trimquotes.TryTrimQuotes(portRaw)

	identityFileRaw := WaveSshConfigUserSettings().GetAll(hostPattern, "IdentityFile")
	for i := 0; i < len(identityFileRaw); i++ {
		identityFileRaw[i] = trimquotes.TryTrimQuotes(identityFileRaw[i])
	}
	sshKeywords.SshIdentityFile = identityFileRaw

	batchModeRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "BatchMode")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshBatchMode = (strings.ToLower(trimquotes.TryTrimQuotes(batchModeRaw)) == "yes")

	// we currently do not support host-bound or unbound but will use yes when they are selected
	pubkeyAuthenticationRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "PubkeyAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshPubkeyAuthentication = (strings.ToLower(trimquotes.TryTrimQuotes(pubkeyAuthenticationRaw)) != "no")

	passwordAuthenticationRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "PasswordAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshPasswordAuthentication = (strings.ToLower(trimquotes.TryTrimQuotes(passwordAuthenticationRaw)) != "no")

	kbdInteractiveAuthenticationRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "KbdInteractiveAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshKbdInteractiveAuthentication = (strings.ToLower(trimquotes.TryTrimQuotes(kbdInteractiveAuthenticationRaw)) != "no")

	// these are parsed as a single string and must be separated
	// these are case sensitive in openssh so they are here too
	preferredAuthenticationsRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "PreferredAuthentications")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshPreferredAuthentications = strings.Split(trimquotes.TryTrimQuotes(preferredAuthenticationsRaw), ",")
	addKeysToAgentRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "AddKeysToAgent")
	if err != nil {
		return nil, err
	}
	sshKeywords.SshAddKeysToAgent = (strings.ToLower(trimquotes.TryTrimQuotes(addKeysToAgentRaw)) == "yes")

	identityAgentRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "IdentityAgent")
	if err != nil {
		return nil, err
	}
	if identityAgentRaw == "" {
		shellPath := shellutil.DetectLocalShellPath()
		authSockCommand := exec.Command(shellPath, "-c", "echo ${SSH_AUTH_SOCK}")
		sshAuthSock, err := authSockCommand.Output()
		if err == nil {
			agentPath, err := wavebase.ExpandHomeDir(trimquotes.TryTrimQuotes(strings.TrimSpace(string(sshAuthSock))))
			if err != nil {
				return nil, err
			}
			sshKeywords.SshIdentityAgent = agentPath
		} else {
			log.Printf("unable to find SSH_AUTH_SOCK: %v\n", err)
		}
	} else {
		agentPath, err := wavebase.ExpandHomeDir(trimquotes.TryTrimQuotes(identityAgentRaw))
		if err != nil {
			return nil, err
		}
		sshKeywords.SshIdentityAgent = agentPath
	}

	proxyJumpRaw, err := WaveSshConfigUserSettings().GetStrict(hostPattern, "ProxyJump")
	if err != nil {
		return nil, err
	}
	proxyJumpSplit := strings.Split(proxyJumpRaw, ",")
	for _, proxyJumpName := range proxyJumpSplit {
		proxyJumpName = strings.TrimSpace(proxyJumpName)
		if proxyJumpName == "" || strings.ToLower(proxyJumpName) == "none" {
			continue
		}
		sshKeywords.SshProxyJump = append(sshKeywords.SshProxyJump, proxyJumpName)
	}
	rawUserKnownHostsFile, _ := WaveSshConfigUserSettings().GetStrict(hostPattern, "UserKnownHostsFile")
	sshKeywords.SshUserKnownHostsFile = strings.Fields(rawUserKnownHostsFile) // TODO - smarter splitting escaped spaces and quotes
	rawGlobalKnownHostsFile, _ := WaveSshConfigUserSettings().GetStrict(hostPattern, "GlobalKnownHostsFile")
	sshKeywords.SshGlobalKnownHostsFile = strings.Fields(rawGlobalKnownHostsFile) // TODO - smarter splitting escaped spaces and quotes

	return sshKeywords, nil
}

type SSHOpts struct {
	SSHHost string `json:"sshhost"`
	SSHUser string `json:"sshuser"`
	SSHPort int    `json:"sshport,omitempty"`
}

func (opts SSHOpts) String() string {
	stringRepr := ""
	if opts.SSHUser != "" {
		stringRepr = opts.SSHUser + "@"
	}
	stringRepr = stringRepr + opts.SSHHost
	if opts.SSHPort != 0 {
		stringRepr = stringRepr + ":" + fmt.Sprint(opts.SSHPort)
	}
	return stringRepr
}
