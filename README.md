# RetrievalBot

The goal of retrieval bot is to offer a scalable framework to perform retrieval testing over Filecoin network. 

There is no centralized orchestrator to manage retrieval queue or work. Instead, it uses MongoDB to manage work queue as well as saving retrieval results.

## Result Snapshot 2024-04-16
The retrieval success ratio and count for each SP per day per protocol has been exported into [result.zip](./result.zip)

Query used to generate the data
```javascript
db.task_result.aggregate([
  {
    $group: {
      _id: {
        sp: "$task.provider.id",
        type: "$task.module",
        date: {
          $dateToString: {
            format: "%Y-%m-%d",
            date: "$created_at",
            timezone: "UTC",
          }
        },
      },
      count: { $sum: 1 },
      success: { $sum: { $cond: [{ $eq: ["$result.success", true] }, 1, 0] } },
    }
  },
  {
    $project: {
      _id: 0,
      sp: "$_id.sp",
      type: "$_id.type",
      date: "$_id.date",
      success: "$success",
      total: "$count",
      ratio: { $divide: ["$success", "$count"] },
    }
  }
])
```

## Workers
Workers refer to the unit that consumes the worker queue. There are 4 basic types of workers as of now.

### Bitswap Worker
This worker currently only support retrieving a single block from the storage provider:
1. Lookup the provider's libp2p protocols
2. If it is using boost market, then lookup the supported retrieval protocols
3. Find the bitswap protocol info and make a single block retrieval

### Graphsync Worker
This worker currently only support retrieving the root block from the storage provider:
1. Make graphsync retrieval with selector that only matches root block from the storage provider

### HTTP Worker
This worker currently only support retrieving the first few MiB of the pieces from the storage provider:
1. Lookup the provider's libp2p protocols
2. If it is using boost market, then lookup the supported retrieval protocols
3. Find the HTTP protocol info and make the retrieval for up to first few MiB

### Stub Worker
This type of worker does nothing but saves random result to the database. It is used to test the database connection and the queue.

## Integrations
Integrations refer to the unit that either pushes work item to the retrieval queue, or other long-running jobs that may interact with the database in different ways

### StateMarketDeals Integration
This integration periodically pulls the statemarketdeals.json from GLIP API and saves it to the database.

### FILPLUS Integration
This integration pulls random active deals from StateMarketDeals database and push Bitswap/Graphsync/HTTP retrieval workitems into the work queue.

## Get started
1. Setup a mongodb server
2. Setup a free ipinfo account and grab a token
3. `make build`
4. Run the software natively or via a docker with environment variables. You need to run three programs:
   1. `statemarketdeals` that pulls statemarketdeals.json from GLIP API and saves it to the database. Check [.env.statemarketdeals](./.env.statemarketdeals) for environment variables.
   2. `filplus_integration` that queues retrieval tasks into a task queue. Check [.env.filplus](./.env.filplus) for environment variables.
   3. `retrieval_worker` that consumes the task queue and performs the retrieval. Check [.env.retrievalworker](./.env.retrievalworker) for environment variables.
5. All programs above will load `.env` file in the working directory so you will need to copy the relevant environment variable file to `.env`
6. When running `retrieval_worker`, you need to make sure `bitswap_worker`, `graphsync_worker`, `http_worker` are in the working directory as well.
