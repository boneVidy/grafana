import React, { PropsWithChildren } from 'react';
import { ElasticDatasource } from '../datasource';
import { from } from 'rxjs';
import { ElasticsearchProvider } from '../components/QueryEditor/ElasticsearchQueryContext';
import { getDefaultTimeRange } from '@grafana/data';
import { ElasticsearchQuery } from '../types';
import { defaultBucketAgg, defaultMetricAgg } from '../query_def';
import { renderHook } from '@testing-library/react-hooks';
import { useFields } from './useFields';
import { MetricAggregationType } from '../components/QueryEditor/MetricAggregationsEditor/aggregations';
import { BucketAggregationType } from '../components/QueryEditor/BucketAggregationsEditor/aggregations';

describe('useFields hook', () => {
  // TODO: If we move the field type to the configuration objects as described in the hook's source
  // we can stop testing for getField to be called with the correct parameters.
  it("returns a function that calls datasource's getFields with the correct parameters", async () => {
    const timeRange = getDefaultTimeRange();
    const query: ElasticsearchQuery = {
      refId: 'A',
      query: '',
      metrics: [defaultMetricAgg()],
      bucketAggs: [defaultBucketAgg()],
    };

    const getFields: ElasticDatasource['getFields'] = jest.fn(() => from([[]]));

    const wrapper = ({ children }: PropsWithChildren<{}>) => (
      <ElasticsearchProvider
        datasource={{ getFields } as ElasticDatasource}
        query={query}
        range={timeRange}
        onChange={() => {}}
        onRunQuery={() => {}}
      >
        {children}
      </ElasticsearchProvider>
    );

    //
    // METRIC AGGREGATIONS
    //
    // Cardinality works on every kind of data
    const { result, rerender } = renderHook(
      (aggregationType: BucketAggregationType | MetricAggregationType) => useFields(aggregationType),
      { wrapper, initialProps: 'cardinality' }
    );
    result.current();
    expect(getFields).toHaveBeenLastCalledWith([], timeRange);

    // All other metric aggregations only work on numbers
    rerender('avg');
    result.current();
    expect(getFields).toHaveBeenLastCalledWith(['number'], timeRange);

    //
    // BUCKET AGGREGATIONS
    //
    // Date Histrogram only works on dates
    rerender('date_histogram');
    result.current();
    expect(getFields).toHaveBeenLastCalledWith(['date'], timeRange);

    // Geohash Grid only works on geo_point data
    rerender('geohash_grid');
    result.current();
    expect(getFields).toHaveBeenLastCalledWith(['geo_point'], timeRange);

    // All other bucket aggregation work on any kind of data
    rerender('terms');
    result.current();
    expect(getFields).toHaveBeenLastCalledWith([], timeRange);

    // top_metrics work on only on numeric data in 7.7
    rerender('top_metrics');
    result.current();
    expect(getFields).toHaveBeenLastCalledWith(['number', 'date'], timeRange);
  });
});
