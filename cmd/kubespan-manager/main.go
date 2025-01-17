// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/talos-systems/kubespan-manager/internal/db"
	"github.com/talos-systems/kubespan-manager/pkg/types"
)

var (
	listenAddr = ":3000"
	devMode    bool
	nodeDB     db.DB
)

func init() {
	flag.StringVar(&listenAddr, "addr", ":3000", "addr on which to listen")
	flag.BoolVar(&devMode, "debug", false, "enable debug mode")
}

//nolint:gocognit,gocyclo,cyclop
func main() {
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalln("failed to initialize logger:", err)
	}

	if os.Getenv("MODE") == "dev" {
		devMode = true
		logger, err = zap.NewDevelopment()

		if err != nil {
			log.Fatalln("failed to initialize development logger:", err)
		}
	}

	if os.Getenv("REDIS_ADDR") != "" {
		nodeDB, err = db.NewRedis(os.Getenv("REDIS_ADDR"), logger)
		if err != nil {
			log.Fatalln("failed to connect to redis: %w", err)
		}
	} else {
		nodeDB = db.New(logger)
	}

	app := fiber.New()

	app.Get("/:cluster", func(c *fiber.Ctx) error {
		cluster := c.Params("cluster")
		if cluster == "" {
			logger.Error("empty cluster for node list")

			return c.SendStatus(http.StatusBadRequest)
		}

		if e := validateClusterID(cluster); e != nil {
			logger.Error("bad cluster ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		list, e := nodeDB.List(c.Context(), cluster)
		if e != nil {
			if errors.Is(e, db.ErrNotFound) {
				logger.Warn("cluster not found",
					zap.String("cluster", cluster),
					zap.Error(e),
				)

				return c.SendStatus(http.StatusNotFound)
			}

			return c.SendStatus(http.StatusInternalServerError)
		}

		logger.Info("listing cluster nodes",
			zap.String("cluster", c.Params("cluster", "")),
			zap.Int("count", len(list)),
		)

		return c.JSON(list)
	})

	app.Get("/:cluster/:node", func(c *fiber.Ctx) error {
		cluster := c.Params("cluster", "")
		if cluster == "" {
			logger.Error("empty cluster for node get")

			return c.SendStatus(http.StatusBadRequest)
		}

		if e := validateClusterID(cluster); e != nil {
			logger.Error("bad cluster ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if e := validatePublicKey(c.Params("node")); e != nil {
			logger.Error("bad node ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", c.Params("node", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		node := c.Params("node", "")
		if node == "" {
			logger.Error("empty node for node get",
				zap.String("cluster", c.Params("cluster", "")),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		n, e := nodeDB.Get(c.Context(), cluster, node)
		if e != nil {
			if errors.Is(e, db.ErrNotFound) {
				logger.Warn("node not found",
					zap.String("cluster", cluster),
					zap.String("node", node),
					zap.Error(err),
				)

				return c.SendStatus(http.StatusNotFound)
			}

			return c.SendStatus(http.StatusInternalServerError)
		}

		logger.Info("returning cluster node",
			zap.String("cluster", c.Params("cluster", "")),
			zap.String("node", n.ID),
			zap.String("ip", n.IP.String()),
			zap.Strings("addresses", addressToString(n.Addresses)),
			zap.Error(err),
		)

		return c.JSON(n)
	})

	// PUT addresses to a Node
	app.Put("/:cluster/:node", func(c *fiber.Ctx) error {
		var addresses []*types.Address

		if e := validateClusterID(c.Params("cluster")); e != nil {
			logger.Error("bad cluster ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if e := validatePublicKey(c.Params("node")); e != nil {
			logger.Error("bad node ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", c.Params("node", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if e := c.BodyParser(&addresses); e != nil {
			logger.Error("failed to parse node PUT",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", c.Params("node", "")),
				zap.Error(e),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		node := c.Params("node", "")
		if node == "" {
			logger.Error("invalid node key",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", c.Params("node", "")),
			)
		}

		if err := nodeDB.AddAddresses(c.Context(), c.Params("cluster", ""), node, addresses...); err != nil {
			logger.Error("failed to add known endpoints",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", node),
				zap.Strings("addresses", addressToString(addresses)),
				zap.Error(err),
			)

			return c.SendStatus(http.StatusInternalServerError)
		}

		return c.SendStatus(http.StatusNoContent)
	})

	app.Post("/:cluster", func(c *fiber.Ctx) error {
		n := new(types.Node)

		if err := validateClusterID(c.Params("cluster")); err != nil {
			logger.Error("bad cluster ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.Error(err),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if err := c.BodyParser(n); err != nil {
			logger.Error("failed to parse node POST",
				zap.String("cluster", c.Params("cluster", "")),
				zap.Error(err),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if err := validatePublicKey(n.ID); err != nil {
			logger.Error("bad node ID",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", n.ID),
				zap.Error(err),
			)

			return c.SendStatus(http.StatusBadRequest)
		}

		if err := nodeDB.Add(c.Context(), c.Params("cluster", ""), n); err != nil {
			logger.Error("failed to add/update node",
				zap.String("cluster", c.Params("cluster", "")),
				zap.String("node", n.ID),
				zap.String("ip", n.IP.String()),
				zap.Strings("addresses", addressToString(n.Addresses)),
				zap.Error(err),
			)

			return c.SendStatus(http.StatusInternalServerError)
		}

		logger.Info("add/update node",
			zap.String("cluster", c.Params("cluster", "")),
			zap.String("node", n.ID),
			zap.String("ip", n.IP.String()),
			zap.Strings("addresses", addressToString(n.Addresses)),
		)

		return c.SendStatus(http.StatusNoContent)
	})

	go func() {
		for {
			time.Sleep(time.Hour)

			nodeDB.Clean()
		}
	}()
	logger.Fatal("listen exited",
		zap.Error(app.Listen(listenAddr)),
	)
}

func addressToString(addresses []*types.Address) (out []string) {
	for _, a := range addresses {
		if !a.IP.IsZero() {
			out = append(out, a.IP.String())

			continue
		}

		out = append(out, a.Name)
	}

	return out
}

func validateClusterID(cluster string) error {
	if _, err := uuid.Parse(cluster); err != nil {
		return fmt.Errorf("cluster ID is not a valid UUID: %w", err)
	}

	return nil
}

func validatePublicKey(key string) error {
	if _, err := wgtypes.ParseKey(key); err != nil {
		return fmt.Errorf("node ID is not a valid wireguard key")
	}

	return nil
}
