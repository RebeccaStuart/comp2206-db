package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fentec-project/gofe/abe"
	"github.com/plzfgme/mfast"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ownerKeys struct {
	ABEPK *abe.FAMEPubKey
	ABESK *abe.FAMESecKey
}
type Owner struct {
	mfastOwner *mfast.Owner
	aBE        *abe.FAME
	keys       *ownerKeys
	conn       *grpc.ClientConn
	client     ServerServiceClient
	config     *OwnerConfig
	logger     Logger
}

type OwnerConfig struct {
	StorePath  string   `json:"store_path,omitempty"`
	SetList    []string `json:"set_list,omitempty"`
	ServerAddr string   `json:"server_addr,omitempty"`
	Logger     Logger   `json:"-"`
}

func ReadOwnerConfig(path string) (*OwnerConfig, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	config := &OwnerConfig{}
	err = json.Unmarshal(file, config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func NewOwner(config *OwnerConfig) (*Owner, error) {
	mfastConfig := &mfast.OwnerConfig{
		StorePath: filepath.Join(config.StorePath, "mfast"),
		SetList:   config.SetList,
	}
	mfastOwner, err := mfast.NewOwner(mfastConfig)
	if err != nil {
		return nil, err
	}
	aBE := abe.NewFAME()
	keys := &ownerKeys{}
	keysPath := filepath.Join(config.StorePath, "keys")
	if _, err := os.Stat(keysPath); os.IsNotExist(err) {
		keys.ABEPK, keys.ABESK, err = aBE.GenerateMasterKeys()
		if err != nil {
			return nil, err
		}
		jsonKeys, err := json.Marshal(keys)
		if err != nil {
			return nil, err
		}
		err = os.WriteFile(keysPath, jsonKeys, 0600)
		if err != nil {
			return nil, err
		}
	} else {
		jsonKeys, err := os.ReadFile(keysPath)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(jsonKeys, keys)
		if err != nil {
			return nil, err
		}
	}
	conn, err := grpc.Dial(config.ServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	client := NewServerServiceClient(conn)

	return &Owner{
		mfastOwner: mfastOwner,
		aBE:        aBE,
		keys:       keys,
		conn:       conn,
		client:     client,
		config:     config,
		logger:     config.Logger,
	}, err
}

func (owner *Owner) Close() error {
	owner.conn.Close()
	return owner.mfastOwner.Close()
}

func (owner *Owner) Insert(ctx context.Context, recs []*Record) error {
	docs := make([]*Document, len(recs))
	tkns := make([]*Token, 0)
	for i, rec := range recs {
		if owner.logger != nil {
			owner.logger.Info(fmt.Sprintf("正在加密记录 %v", rec))
		}

		newRec := bson.M{}
		objId := primitive.NewObjectID()
		newRec["_id"] = objId
		partTkns, err := owner.retriveTkns(objId, rec)
		if err != nil {
			return err
		}
		tkns = append(tkns, partTkns...)
		msp, err := abe.BooleanToMSP(rec.Set, false)
		if err != nil {
			return err
		}
		newUserId, err := owner.aBE.Encrypt(rec.UserId, msp, owner.keys.ABEPK)
		if err != nil {
			return err
		}
		bUid, err := json.Marshal(newUserId)
		if err != nil {
			return err
		}
		newRec["UserId"] = bUid
		newLoc, err := owner.aBE.Encrypt(rec.Location, msp, owner.keys.ABEPK)
		if err != nil {
			return err
		}
		bLoc, err := json.Marshal(newLoc)
		if err != nil {
			return err
		}
		newRec["Location"] = bLoc
		newTime, err := owner.aBE.Encrypt(rec.Time.Format(time.RFC1123), msp, owner.keys.ABEPK)
		if err != nil {
			return err
		}
		bTime, err := json.Marshal(newTime)
		if err != nil {
			return err
		}
		newRec["Time"] = bTime
		newSet, err := owner.aBE.Encrypt(rec.Set, msp, owner.keys.ABEPK)
		if err != nil {
			return err
		}
		bSet, err := json.Marshal(newSet)
		if err != nil {
			return err
		}
		newRec["Set"] = bSet

		if owner.logger != nil {
			owner.logger.Info(fmt.Sprintf("加密后的记录 %v", newRec))
		}

		bytesRec, err := bson.Marshal(newRec)
		if err != nil {
			return err
		}

		docs[i] = &Document{
			Binary: bytesRec,
		}
	}

	if owner.logger != nil {
		owner.logger.Info("插入记录中")
	}
	res, err := owner.client.Insert(ctx, &InsertQuery{
		Tkns: tkns,
		Docs: docs,
	})
	if err != nil {
		if owner.logger != nil {
			owner.logger.Error("插入记录失败，错误：" + err.Error())
		}
		return err
	} else if res.Msg != "Ok" {
		if owner.logger != nil {
			owner.logger.Error("插入记录失败，错误：" + res.Msg)
		}
		return errors.New(res.Msg)
	}
	if owner.logger != nil {
		owner.logger.Info("插入记录成功")
	}

	return nil
}

func (owner *Owner) FindB(ctx context.Context, set string, loc string, timeA, timeB time.Time) ([]*FindBResult, error) {
	tKWs := getBRCKWs(uint64(timeA.Unix()), uint64(timeB.Unix()))
	kws := make([][]byte, len(tKWs))
	for i, tKW := range tKWs {
		kws[i] = []byte("B:" + loc + ":" + tKW)
	}

	findTkns := make([]*Token, 0)
	if owner.logger != nil {
		owner.logger.Info("生成查询陷门中")
	}
	for i := range kws {
		searchTkn, err := owner.mfastOwner.GenSearchTkn(set, kws[i])
		if err != nil {
			return nil, err
		} else if searchTkn == nil {
			continue
		}
		b, err := bson.Marshal(searchTkn)
		if err != nil {
			return nil, err
		}
		findTkns = append(findTkns, &Token{Binary: b})
	}
	if owner.logger != nil {
		owner.logger.Info("生成查询陷门: " + fmt.Sprintf("%v", findTkns))
	}

	findQ := &FindQuery{
		Fields: []string{"UserId"},
		Tkns:   findTkns,
	}
	if owner.logger != nil {
		owner.logger.Info("查询记录中")
	}
	findRes, err := owner.client.Find(ctx, findQ)
	if err != nil {
		if owner.logger != nil {
			owner.logger.Error("查询记录失败，错误：" + err.Error())
		}
		return nil, err
	} else if findRes.GetMsg() != "Ok" {
		if owner.logger != nil {
			owner.logger.Error("查询记录失败，错误：" + findRes.GetMsg())
		}
		return nil, errors.New(findRes.GetMsg())
	}
	if owner.logger != nil {
		owner.logger.Info("查询记录成功")
	}

	bDocs := findRes.GetDocs()
	results := make([]*FindBResult, len(bDocs))
	for i, bDoc := range bDocs {
		doc := bson.M{}
		err := bson.Unmarshal(bDoc.GetBinary(), doc)
		if err != nil {
			return nil, err
		}
		if owner.logger != nil {
			owner.logger.Info(fmt.Sprintf("加密记录 %v", doc))
		}
		cUserId := &abe.FAMECipher{}
		json.Unmarshal(doc["UserId"].(primitive.Binary).Data, cUserId)
		abeAttrK, err := owner.aBE.GenerateAttribKeys([]string{set}, owner.keys.ABESK)
		if err != nil {
			return nil, err
		}
		userId, _ := owner.aBE.Decrypt(cUserId, abeAttrK, owner.keys.ABEPK)

		results[i] = &FindBResult{
			UserId: userId,
		}
		if owner.logger != nil {
			owner.logger.Info("解密后的记录：" + fmt.Sprintf("%v", results[i]))
		}
	}
	return results, nil
}

func (owner *Owner) FindB2(ctx context.Context, set string, loc string, timeA, timeB time.Time) ([]*FindB2Result, error) {
	tKWs := getBRCKWs(uint64(timeA.Unix()), uint64(timeB.Unix()))
	kws := make([][]byte, len(tKWs))
	for i, tKW := range tKWs {
		kws[i] = []byte("B:" + loc + ":" + tKW)
	}

	findTkns := make([]*Token, 0)
	if owner.logger != nil {
		owner.logger.Info("生成查询陷门中")
	}
	for i := range kws {
		searchTkn, err := owner.mfastOwner.GenSearchTkn(set, kws[i])
		if err != nil {
			return nil, err
		} else if searchTkn == nil {
			continue
		}
		b, err := bson.Marshal(searchTkn)
		if err != nil {
			return nil, err
		}
		findTkns = append(findTkns, &Token{Binary: b})
	}
	if owner.logger != nil {
		owner.logger.Info("生成查询陷门: " + fmt.Sprintf("%v", findTkns))
	}

	findQ := &FindQuery{
		Fields: []string{"UserId", "Set"},
		Tkns:   findTkns,
	}
	if owner.logger != nil {
		owner.logger.Info("查询记录中")
	}
	findRes, err := owner.client.Find(ctx, findQ)
	if err != nil {
		if owner.logger != nil {
			owner.logger.Error("查询记录失败，错误：" + err.Error())
		}
		return nil, err
	} else if findRes.GetMsg() != "Ok" {
		if owner.logger != nil {
			owner.logger.Error("查询记录失败，错误：" + findRes.GetMsg())
		}
		return nil, errors.New(findRes.GetMsg())
	}
	if owner.logger != nil {
		owner.logger.Info("查询记录成功")
	}

	bDocs := findRes.GetDocs()
	results := make([]*FindB2Result, len(bDocs))
	for i, bDoc := range bDocs {
		doc := bson.M{}
		err := bson.Unmarshal(bDoc.GetBinary(), doc)
		if err != nil {
			return nil, err
		}
		if owner.logger != nil {
			owner.logger.Info(fmt.Sprintf("加密记录 %v", doc))
		}
		cUserId := &abe.FAMECipher{}
		json.Unmarshal(doc["UserId"].(primitive.Binary).Data, cUserId)
		cSet := &abe.FAMECipher{}
		json.Unmarshal(doc["Set"].(primitive.Binary).Data, cSet)
		abeAttrK, err := owner.aBE.GenerateAttribKeys([]string{set}, owner.keys.ABESK)
		if err != nil {
			return nil, err
		}
		userId, _ := owner.aBE.Decrypt(cUserId, abeAttrK, owner.keys.ABEPK)
		set, _ := owner.aBE.Decrypt(cSet, abeAttrK, owner.keys.ABEPK)

		results[i] = &FindB2Result{
			UserId: userId,
			Set:    set,
		}
		if owner.logger != nil {
			owner.logger.Info("解密后的记录：" + fmt.Sprintf("%v", results[i]))
		}
	}
	return results, nil
}

func (owner *Owner) DelegateKeys(set string) (*DelegatedKeys, error) {
	abeAttrK, err := owner.aBE.GenerateAttribKeys([]string{set}, owner.keys.ABESK)
	if err != nil {
		return nil, err
	}

	mfastKeys, err := owner.mfastOwner.DelegateKeys(set)
	if err != nil {
		return nil, err
	}

	return &DelegatedKeys{
		Set:       set,
		MFastKeys: mfastKeys,
		ABEAttrK:  abeAttrK,
		ABEPK:     owner.keys.ABEPK,
	}, nil
}

func (owner *Owner) GenSearcherConfig(set string) (string, error) {
	dk, err := owner.DelegateKeys(set)
	if err != nil {
		return "", err
	}
	cfg := &SearcherConfig{
		SetList:    owner.config.SetList,
		Keys:       dk,
		ServerAddr: owner.config.ServerAddr,
	}
	jsonCfg, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return "", err
	}
	return string(jsonCfg), nil
}

func (owner *Owner) retriveTkns(id primitive.ObjectID, rec *Record) ([]*Token, error) {
	kws := make([]string, 64)
	intTime := uint64(rec.Time.Unix())
	for i := 0; i < 64; i++ {
		kws[i] = makeTimeKW(intTime>>i, i)
	}
	tkns := make([]*Token, 0, 2*len(kws))
	for _, kw := range kws {
		mfastTknA, err := owner.mfastOwner.GenUpdateTkn(id.Hex(), rec.Set, []byte("A:"+rec.UserId+":"+kw), "add")
		if err != nil {
			return nil, err
		}
		bMFastTknA, err := json.Marshal(mfastTknA)
		if err != nil {
			return nil, err
		}
		mfastTknB, err := owner.mfastOwner.GenUpdateTkn(id.Hex(), rec.Set, []byte("B:"+rec.Location+":"+kw), "add")
		if err != nil {
			return nil, err
		}
		bMFastTknB, err := json.Marshal(mfastTknB)
		if err != nil {
			return nil, err
		}
		tkns = append(tkns, &Token{Binary: bMFastTknA}, &Token{Binary: bMFastTknB})
	}

	return tkns, nil
}
