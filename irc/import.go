// Copyright (c) 2020 Shivaram Lingamneni <slingamn@cs.stanford.edu>
// released under the MIT license

package irc

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"

	"github.com/tidwall/buntdb"

	"github.com/oragono/oragono/irc/utils"
)

const (
	// produce a hardcoded version of the database schema
	// XXX instead of referencing, e.g., keyAccountExists, we should write in the string literal
	// (to ensure that no matter what code changes happen elsewhere, we're still producing a
	// db of the hardcoded version)
	importDBSchemaVersion = 18
)

type userImport struct {
	Name            string
	Hash            string
	Email           string
	RegisteredAt    int64 `json:"registeredAt"`
	Vhost           string
	AdditionalNicks []string `json:"additionalNicks"`
}

type channelImport struct {
	Name         string
	Founder      string
	RegisteredAt int64 `json:"registeredAt"`
	Topic        string
	TopicSetBy   string `json:"topicSetBy"`
	TopicSetAt   int64  `json:"topicSetAt"`
	Amode        map[string]string
	Modes        string
	Key          string
	Limit        int
}

type databaseImport struct {
	Version  int
	Source   string
	Users    map[string]userImport
	Channels map[string]channelImport
}

func serializeAmodes(raw map[string]string) (result []byte, err error) {
	processed := make(map[string]int, len(raw))
	for accountName, mode := range raw {
		if len(mode) != 1 {
			return nil, fmt.Errorf("invalid mode %s for account %s", mode, accountName)
		}
		cfname, err := CasefoldName(accountName)
		if err != nil {
			return nil, fmt.Errorf("invalid amode recipient %s: %w", accountName, err)
		}
		processed[cfname] = int(mode[0])
	}
	result, err = json.Marshal(processed)
	return
}

func doImportDBGeneric(config *Config, dbImport databaseImport, credsType CredentialsVersion, tx *buntdb.Tx) (err error) {
	requiredVersion := 1
	if dbImport.Version != requiredVersion {
		return fmt.Errorf("unsupported version of the db for import: version %d is required", requiredVersion)
	}

	tx.Set(keySchemaVersion, strconv.Itoa(importDBSchemaVersion), nil)
	tx.Set(keyCloakSecret, utils.GenerateSecretKey(), nil)

	for username, userInfo := range dbImport.Users {
		cfUsername, err := CasefoldName(username)
		if err != nil {
			log.Printf("invalid username %s: %v", username, err)
			continue
		}
		credentials := AccountCredentials{
			Version:        credsType,
			PassphraseHash: []byte(userInfo.Hash),
		}
		marshaledCredentials, err := json.Marshal(&credentials)
		if err != nil {
			log.Printf("invalid credentials for %s: %v", username, err)
			continue
		}
		tx.Set(fmt.Sprintf(keyAccountExists, cfUsername), "1", nil)
		tx.Set(fmt.Sprintf(keyAccountVerified, cfUsername), "1", nil)
		tx.Set(fmt.Sprintf(keyAccountName, cfUsername), userInfo.Name, nil)
		tx.Set(fmt.Sprintf(keyAccountCallback, cfUsername), "mailto:"+userInfo.Email, nil)
		tx.Set(fmt.Sprintf(keyAccountCredentials, cfUsername), string(marshaledCredentials), nil)
		tx.Set(fmt.Sprintf(keyAccountRegTime, cfUsername), strconv.FormatInt(userInfo.RegisteredAt, 10), nil)
		if userInfo.Vhost != "" {
			tx.Set(fmt.Sprintf(keyAccountVHost, cfUsername), userInfo.Vhost, nil)
		}
		if len(userInfo.AdditionalNicks) != 0 {
			tx.Set(fmt.Sprintf(keyAccountAdditionalNicks, cfUsername), marshalReservedNicks(userInfo.AdditionalNicks), nil)
		}
	}

	for chname, chInfo := range dbImport.Channels {
		cfchname, err := CasefoldChannel(chname)
		if err != nil {
			log.Printf("invalid channel name %s: %v", chname, err)
			continue
		}
		cffounder, err := CasefoldName(chInfo.Founder)
		if err != nil {
			log.Printf("invalid founder %s for channel %s: %v", chInfo.Founder, chname, err)
			continue
		}
		tx.Set(fmt.Sprintf(keyChannelExists, cfchname), "1", nil)
		tx.Set(fmt.Sprintf(keyChannelName, cfchname), chname, nil)
		tx.Set(fmt.Sprintf(keyChannelRegTime, cfchname), strconv.FormatInt(chInfo.RegisteredAt, 10), nil)
		tx.Set(fmt.Sprintf(keyChannelFounder, cfchname), cffounder, nil)
		accountChannelsKey := fmt.Sprintf(keyAccountChannels, cffounder)
		founderChannels, fcErr := tx.Get(accountChannelsKey)
		if fcErr != nil || founderChannels == "" {
			founderChannels = cfchname
		} else {
			founderChannels = fmt.Sprintf("%s,%s", founderChannels, cfchname)
		}
		tx.Set(accountChannelsKey, founderChannels, nil)
		if chInfo.Topic != "" {
			tx.Set(fmt.Sprintf(keyChannelTopic, cfchname), chInfo.Topic, nil)
			tx.Set(fmt.Sprintf(keyChannelTopicSetTime, cfchname), strconv.FormatInt(chInfo.TopicSetAt, 10), nil)
			tx.Set(fmt.Sprintf(keyChannelTopicSetBy, cfchname), chInfo.TopicSetBy, nil)
		}
		if len(chInfo.Amode) != 0 {
			m, err := serializeAmodes(chInfo.Amode)
			if err == nil {
				tx.Set(fmt.Sprintf(keyChannelAccountToUMode, cfchname), string(m), nil)
			} else {
				log.Printf("couldn't serialize amodes for %s: %v", chname, err)
			}
		}
		tx.Set(fmt.Sprintf(keyChannelModes, cfchname), chInfo.Modes, nil)
		if chInfo.Key != "" {
			tx.Set(fmt.Sprintf(keyChannelPassword, cfchname), chInfo.Key, nil)
		}
		if chInfo.Limit > 0 {
			tx.Set(fmt.Sprintf(keyChannelUserLimit, cfchname), strconv.Itoa(chInfo.Limit), nil)
		}
	}

	return nil
}

func doImportDB(config *Config, dbImport databaseImport, tx *buntdb.Tx) (err error) {
	switch dbImport.Source {
	case "atheme":
		return doImportDBGeneric(config, dbImport, CredentialsAtheme, tx)
	case "anope":
		return doImportDBGeneric(config, dbImport, CredentialsAnope, tx)
	default:
		return fmt.Errorf("unsupported import source: %s", dbImport.Source)
	}
}

func ImportDB(config *Config, infile string) (err error) {
	data, err := ioutil.ReadFile(infile)
	if err != nil {
		return
	}

	var dbImport databaseImport
	err = json.Unmarshal(data, &dbImport)
	if err != nil {
		return err
	}

	err = checkDBReadyForInit(config.Datastore.Path)
	if err != nil {
		return err
	}

	db, err := buntdb.Open(config.Datastore.Path)
	if err != nil {
		return err
	}

	performImport := func(tx *buntdb.Tx) (err error) {
		return doImportDB(config, dbImport, tx)
	}

	return db.Update(performImport)
}
