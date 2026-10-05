// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

import type { NextApiRequest, NextApiResponse } from 'next';
import InstrumentationMiddleware from '../../../utils/telemetry/InstrumentationMiddleware';
import { Empty, Product } from '../../../protos/demo';
import ProductCatalogService from '../../../services/ProductCatalog.service';
import ProductCatalogGateway from '../../../gateways/rpc/ProductCatalog.gateway';

// Adding products is for the catalog load test only: it is off unless DEMO_ADMIN_TOKEN is
// set, and then needs the same value in the X-Demo-Token header.
const { DEMO_ADMIN_TOKEN = '' } = process.env;

type TResponse = Product[] | Empty;

const handler = async ({ method, query, headers, body }: NextApiRequest, res: NextApiResponse<TResponse>) => {
  switch (method) {
    case 'GET': {
      const { currencyCode = '' } = query;
      const productList = await ProductCatalogService.listProducts(currencyCode as string);

      return res.status(200).json(productList);
    }

    case 'POST': {
      if (!DEMO_ADMIN_TOKEN || headers['x-demo-token'] !== DEMO_ADMIN_TOKEN) {
        return res.status(403).send('');
      }
      const product = await ProductCatalogGateway.addProduct(body as Product);

      return res.status(201).json([product]);
    }

    default: {
      return res.status(405).send('');
    }
  }
};

export default InstrumentationMiddleware(handler);
