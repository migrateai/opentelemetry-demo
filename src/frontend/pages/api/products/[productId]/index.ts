// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

import type { NextApiRequest, NextApiResponse } from 'next';
import InstrumentationMiddleware from '../../../../utils/telemetry/InstrumentationMiddleware';
import { Empty, Product } from '../../../../protos/demo';
import ProductCatalogService from '../../../../services/ProductCatalog.service';
import { status, ServiceError } from '@grpc/grpc-js';

type TResponse = Product | Empty;

const handler = async ({ method, query }: NextApiRequest, res: NextApiResponse<TResponse>) => {
  switch (method) {
    case 'GET': {
      const { productId = '', currencyCode = '' } = query;
      try {
        const product = await ProductCatalogService.getProduct(productId as string, currencyCode as string);
        return res.status(200).json(product);
      } catch (error) {
        const grpcError = error as ServiceError;
        // Map gRPC error codes to HTTP status codes
        switch (grpcError.code) {
          case status.NOT_FOUND:
            return res.status(404).json({ error: grpcError.message });
          case status.INVALID_ARGUMENT:
            return res.status(400).json({ error: grpcError.message });
          case status.UNAVAILABLE:
            return res.status(503).json({ error: grpcError.message });
          default:
            console.error('Unhandled gRPC error:', grpcError);
            return res.status(500).json({ error: grpcError.message });
        }
      }
    }

    default: {
      return res.status(405).send('');
    }
  }
};

export default InstrumentationMiddleware(handler);
