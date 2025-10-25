# Grimoire API Structure

The Grimoire application has been refactored into a clean, modular structure with separated concerns.

## Package Structure

```
Grimoire/
├── cmd/
│   ├── web-server/
│   │   └── main.go         # Web server for static files and UI
│   └── api-server/
│       ├── main.go         # API server entry point
│       └── handlers.go     # HTTP API handlers and job management
├── internal/
│   └── jobs/
│       └── job.go          # Core job processing logic
├── web/static/             # Static web assets
└── example_usage.go        # Usage examples
```

## Key Components

### 1. Web Server (`cmd/web-server/main.go`)
- **Purpose**: Serves static files and web UI
- **Port**: 8080
- **Responsibilities**: 
  - Serve static files (CSS, JS, images)
  - Serve HTML templates
  - Handle web page requests

### 2. API Server (`cmd/api-server/`)
- **Purpose**: Handles all API requests and job management
- **Port**: 8081
- **Responsibilities**:
  - HTTP request/response handling
  - Job creation and tracking
  - API route configuration
  - Job status management
  - PDF generation and download

### 3. Jobs Package (`internal/jobs/job.go`)
- **Purpose**: Core business logic for decklist processing
- **Responsibilities**:
  - Card parsing and validation
  - Scryfall API integration
  - PDF generation
  - Rate limiting
  - Job lifecycle management

## Server Endpoints

### Web Server (Port 8080)
- `GET /` - Home page
- `GET /submit` - Job submission page
- `GET /static/*` - Static assets (CSS, images, JS)

### API Server (Port 8081)
- `POST /api/submit` - Submit a decklist for processing
- `GET /api/{id}` - Get job status
- `GET /api/{id}/pdf` - Download PDF when complete
- `GET /api/jobs` - List all jobs

## Usage Examples

### Direct Job Usage
```go
import "Grimoire/internal/jobs"

// Create and start a job
job := jobs.NewGrimoireJob(decklist)
job.SubmitDecklist()

// Check status
status, err := job.GetStatus()
if status == "complete" {
    // Job is done!
}
```

### HTTP API Usage
```bash
# Submit a job
curl -X POST http://localhost:8081/api/submit -d "Decklist=1 Forest (iko) 258"

# Check status
curl http://localhost:8081/api/job_1234567890

# Download PDF when complete
curl http://localhost:8081/api/job_1234567890/pdf -o decklist.pdf
```

### Running the Servers
```bash
# Start the web server (serves UI)
go run cmd/web-server/main.go

# Start the API server (handles API requests)
go run cmd/api-server/main.go
```

## Job Status

1. **`parse`** - Parsing decklist lines
2. **`fetch`** - Fetching card data from Scryfall API
3. **`generate`** - Generating PDF
4. **`complete`** - Job finished successfully
5. **`error`** - Job failed with error

## Benefits of This Structure

- **Separation of Concerns**: Each package has a single responsibility
- **Modularity**: Easy to test and maintain individual components
- **Scalability**: Can easily add new features or modify existing ones
- **Reusability**: Core job logic can be used independently of HTTP layer
- **Clean Dependencies**: Clear import relationships between packages
