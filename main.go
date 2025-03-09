package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {
	// Parse command line flags
	daemonMode := flag.Bool("d", false, "Run in daemon mode (background)")
	flag.BoolVar(daemonMode, "daemon", false, "Run in daemon mode (background)")
	flag.Parse()

	if *daemonMode {
		// Fork the process
		args := append([]string{os.Args[0]}, flag.Args()...)
		procAttr := &os.ProcAttr{
			Dir: ".",
			Env: os.Environ(),
			Files: []*os.File{
				os.Stdin,
				nil, // stdout goes to /dev/null
				nil, // stderr goes to /dev/null
			},
		}

		process, err := os.StartProcess(args[0], args, procAttr)
		if err != nil {
			log.Fatalf("Error starting daemon process: %v", err)
		}

		// Parent process exits
		process.Release()
		os.Exit(0)
	}

	log.Println("Application starting...")

	// Create Kubernetes client
	log.Println("Creating Kubernetes clients...")
	clientset, metricsClient, err := createClients()
	if err != nil {
		log.Fatalf("Error creating Kubernetes clients: %v", err)
	}
	log.Println("Successfully created Kubernetes clients")

	// Initialize database connection
	log.Println("Initializing database connection...")
	db, err := initDatabase()
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}
	log.Println("Successfully connected to database")
	defer db.Close()

	// Start web server in a goroutine
	log.Println("Starting web server...")
	go func() {
		port := getEnvWithDefault("WEB_PORT", "8080")
		address := "192.168.13.1:" + port
		log.Printf("Web server will listen on %s", address)
		if err := startWebServer(db, port); err != nil {
			log.Printf("Web server error: %v", err)
		}
	}()

	// Get all namespaces
	log.Println("Fetching namespaces from Kubernetes...")
	namespaces, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Error getting namespaces: %v", err)
	}
	log.Printf("Found %d namespaces", len(namespaces.Items))

	log.Println("Starting pod metrics collector...")
	log.Printf("Web interface available at http://localhost:%s/metrics", getEnvWithDefault("WEB_PORT", "8080"))

	// Continuously collect metrics
	for {
		log.Println("Collecting pod metrics...")
		timestamp := time.Now()

		// Iterate through each namespace
		for _, ns := range namespaces.Items {
			namespace := ns.Name

			// Get pod metrics for this namespace
			podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(namespace).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				log.Printf("Error getting pod metrics for namespace %s: %v", namespace, err)
				continue
			}

			// Store metrics for each pod
			for _, podMetric := range podMetrics.Items {
				podName := podMetric.Name

				// Calculate total CPU and memory usage for the pod
				var cpuTotal int64
				var memoryTotal int64

				for _, container := range podMetric.Containers {
					cpuQuantity := container.Usage.Cpu().MilliValue()
					memoryQuantity := container.Usage.Memory().Value()

					cpuTotal += cpuQuantity
					memoryTotal += memoryQuantity
				}

				// Convert millicores to cores for storage
				cpuCores := float64(cpuTotal) / 1000.0

				// Store metrics in database
				err = storePodMetrics(db, timestamp, podName, namespace, cpuCores, memoryTotal)
				if err != nil {
					log.Printf("Error storing metrics for pod %s/%s: %v", namespace, podName, err)
				}
			}
		}

		log.Println("Metrics collection completed. Sleeping for 10 seconds before next collection...")
		time.Sleep(10 * time.Second)
	}
}

// createClients creates the Kubernetes and metrics clients
func createClients() (*kubernetes.Clientset, *versioned.Clientset, error) {
	var config *rest.Config
	var err error

	log.Println("Attempting to create Kubernetes config...")

	// Try in-cluster config first
	log.Println("Trying in-cluster configuration...")
	config, err = rest.InClusterConfig()
	if err != nil {
		log.Println("Not running in cluster, trying kubeconfig file...")
		// Fall back to kubeconfig
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		log.Printf("Looking for kubeconfig at: %s", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create Kubernetes config: %v", err)
		}
		log.Println("Successfully loaded kubeconfig file")
	} else {
		log.Println("Successfully loaded in-cluster configuration")
	}

	// Create the clientset for accessing Kubernetes API
	log.Println("Creating Kubernetes clientset...")
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("could not create Kubernetes client: %v", err)
	}
	log.Println("Successfully created Kubernetes clientset")

	// Create the metrics clientset
	log.Println("Creating Metrics clientset...")
	metricsClient, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("could not create Metrics client: %v", err)
	}
	log.Println("Successfully created Metrics clientset")

	return clientset, metricsClient, nil
}

// initDatabase initializes the database connection and creates the necessary tables
func initDatabase() (*sql.DB, error) {
	// Get database configuration from environment variables
	dbType := getEnvWithDefault("DB_TYPE", "sqlite3")
	dbConnection := getEnvWithDefault("DB_CONNECTION", "pod_metrics.db")

	// Connect to the database
	db, err := sql.Open(dbType, dbConnection)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %v", err)
	}

	// Create the metrics table if it doesn't exist
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS pod_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		pod_name TEXT NOT NULL,
		namespace TEXT NOT NULL,
		cpu_cores REAL NOT NULL,
		memory_bytes BIGINT NOT NULL
	);
	`

	// Adjust SQL syntax for PostgreSQL
	if dbType == "postgres" {
		createTableSQL = `
		CREATE TABLE IF NOT EXISTS pod_metrics (
			id SERIAL PRIMARY KEY,
			timestamp TIMESTAMP NOT NULL,
			pod_name TEXT NOT NULL,
			namespace TEXT NOT NULL,
			cpu_cores REAL NOT NULL,
			memory_bytes BIGINT NOT NULL
		);
		`
	}

	_, err = db.Exec(createTableSQL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create metrics table: %v", err)
	}

	return db, nil
}

// storePodMetrics stores pod metrics in the database
func storePodMetrics(db *sql.DB, timestamp time.Time, podName, namespace string, cpuCores float64, memoryBytes int64) error {
	query := `
	INSERT INTO pod_metrics (timestamp, pod_name, namespace, cpu_cores, memory_bytes)
	VALUES (?, ?, ?, ?, ?)
	`

	_, err := db.Exec(query, timestamp, podName, namespace, cpuCores, memoryBytes)
	return err
}

// getEnvWithDefault returns the value of an environment variable or a default value if not set
func getEnvWithDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// PodMetric represents a single pod metric record for display
type PodMetric struct {
	Timestamp   string
	PodName     string
	Namespace   string
	CPUCores    float64
	MemoryMB    float64
	MaxCPUCores float64
	MaxMemoryMB float64
}

// startWebServer starts a web server to display pod metrics
func startWebServer(db *sql.DB, port string) error {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/metrics", http.StatusSeeOther)
	})

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics, err := getPodMetrics(db)
		if err != nil {
			http.Error(w, "Failed to retrieve metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}

		tmpl := `
<!DOCTYPE html>
<html>
<head>
    <title>Pod Metrics</title>
    <style>
        :root {
            --bg-color: #1a1a1a;
            --text-color: #e0e0e0;
            --border-color: #333;
            --hover-color: #2d2d2d;
            --link-color: #66b3ff;
            --header-bg: #252525;
            --row-alt-bg: #222;
            --input-bg: #333;
            --max-value-color: #888;
        }

        body { 
            font-family: Arial, sans-serif; 
            margin: 20px;
            background-color: var(--bg-color);
            color: var(--text-color);
        }
        h1 { color: var(--text-color); }
        .search-container {
            margin: 20px 0;
            display: flex;
            align-items: center;
        }
        .search-input {
            padding: 8px 12px;
            font-size: 14px;
            border: 1px solid var(--border-color);
            border-radius: 4px;
            width: 300px;
            margin-left: 10px;
            background-color: var(--input-bg);
            color: var(--text-color);
        }
        .search-input:focus {
            outline: none;
            border-color: var(--link-color);
            box-shadow: 0 0 5px rgba(102,179,255,0.2);
        }
        table { 
            border-collapse: collapse; 
            width: 100%;
            margin-top: 20px;
        }
        th, td { 
            border: 1px solid var(--border-color); 
            padding: 8px; 
            text-align: left; 
        }
        th { 
            background-color: var(--header-bg);
            cursor: pointer;
            user-select: none;
        }
        th:hover {
            background-color: var(--hover-color);
        }
        tr:nth-child(even) { background-color: var(--row-alt-bg); }
        tr { transition: opacity 0.2s ease-in-out; }
        tr.hidden { display: none; }
        a { color: var(--link-color); text-decoration: none; }
        a:hover { text-decoration: underline; }
        .no-results {
            text-align: center;
            padding: 20px;
            color: var(--max-value-color);
            font-style: italic;
            display: none;
        }
        .max-value {
            color: var(--max-value-color);
            font-size: 0.9em;
        }
        .sort-menu {
            position: absolute;
            background: var(--bg-color);
            border: 1px solid var(--border-color);
            border-radius: 4px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.3);
            display: none;
            z-index: 1000;
        }
        .sort-menu div {
            padding: 8px 12px;
            cursor: pointer;
        }
        .sort-menu div:hover {
            background-color: var(--hover-color);
        }
    </style>
</head>
<body>
    <h1>Pod Metrics</h1>
    
    <div class="search-container">
        <label for="podSearch">Search pods:</label>
        <input type="text" id="podSearch" class="search-input" placeholder="Type to filter pods...">
    </div>

    <div id="noResults" class="no-results">
        No pods found matching your search
    </div>

    <div id="sortMenu" class="sort-menu">
        <div data-sort="current">Sort by current value</div>
        <div data-sort="max">Sort by max value</div>
    </div>

    <table>
        <thead>
            <tr>
                <th>Timestamp</th>
                <th>Pod Name</th>
                <th>Namespace</th>
                <th class="sortable" data-column="cpu">CPU Cores</th>
                <th class="sortable" data-column="memory">Memory (MB)</th>
            </tr>
        </thead>
        <tbody id="metricsTable">
            {{range .}}
            <tr class="metric-row" 
                data-cpu="{{.CPUCores}}" 
                data-max-cpu="{{.MaxCPUCores}}"
                data-memory="{{.MemoryMB}}"
                data-max-memory="{{.MaxMemoryMB}}">
                <td>{{.Timestamp}}</td>
                <td><a href="/pod/{{.Namespace}}/{{.PodName}}">{{.PodName}}</a></td>
                <td>{{.Namespace}}</td>
                <td>{{printf "%.3f" .CPUCores}} <span class="max-value">(max: {{printf "%.3f" .MaxCPUCores}})</span></td>
                <td>{{printf "%.1f" .MemoryMB}} <span class="max-value">(max: {{printf "%.1f" .MaxMemoryMB}})</span></td>
            </tr>
            {{end}}
        </tbody>
    </table>

    <script>
        // Search functionality
        document.getElementById('podSearch').addEventListener('input', function(e) {
            const searchTerm = e.target.value.toLowerCase();
            const rows = document.getElementsByClassName('metric-row');
            let visibleCount = 0;

            for (let row of rows) {
                const podName = row.getElementsByTagName('td')[1].textContent.toLowerCase();
                if (podName.includes(searchTerm)) {
                    row.classList.remove('hidden');
                    visibleCount++;
                } else {
                    row.classList.add('hidden');
                }
            }

            const noResults = document.getElementById('noResults');
            noResults.style.display = visibleCount === 0 ? 'block' : 'none';
        });

        // Sorting functionality
        let currentSortColumn = null;
        let currentSortDirection = 'desc';
        let currentSortType = 'current';
        const sortMenu = document.getElementById('sortMenu');

        function sortTable(column, direction, sortType) {
            const tbody = document.getElementById('metricsTable');
            const rows = Array.from(tbody.getElementsByClassName('metric-row'));
            
            rows.sort((a, b) => {
                let aValue, bValue;
                
                if (sortType === 'current') {
                    aValue = parseFloat(a.dataset[column]);
                    bValue = parseFloat(b.dataset[column]);
                } else {
                    aValue = parseFloat(a.dataset['max' + column.charAt(0).toUpperCase() + column.slice(1)]);
                    bValue = parseFloat(b.dataset['max' + column.charAt(0).toUpperCase() + column.slice(1)]);
                }

                return direction === 'asc' ? aValue - bValue : bValue - aValue;
            });

            rows.forEach(row => tbody.appendChild(row));
        }

        document.querySelectorAll('th.sortable').forEach(th => {
            th.addEventListener('click', function(e) {
                const column = this.dataset.column;
                const rect = this.getBoundingClientRect();
                
                // Show sort menu
                sortMenu.style.display = 'block';
                sortMenu.style.top = (rect.bottom + window.scrollY) + 'px';
                sortMenu.style.left = rect.left + 'px';
                
                // Store the column for the menu handlers
                sortMenu.dataset.column = column;
            });
        });

        // Handle sort menu clicks
        document.querySelectorAll('#sortMenu div').forEach(option => {
            option.addEventListener('click', function() {
                const column = sortMenu.dataset.column;
                const sortType = this.dataset.sort;
                
                // Update sort direction if clicking the same column and type
                if (currentSortColumn === column && currentSortType === sortType) {
                    currentSortDirection = currentSortDirection === 'asc' ? 'desc' : 'asc';
                } else {
                    currentSortDirection = 'desc';
                }
                
                currentSortColumn = column;
                currentSortType = sortType;
                
                // Update header classes
                document.querySelectorAll('th.sortable').forEach(th => {
                    th.classList.remove('sorted-asc', 'sorted-desc');
                    if (th.dataset.column === column) {
                        th.classList.add(currentSortDirection === 'asc' ? 'sorted-asc' : 'sorted-desc');
                    }
                });
                
                sortTable(column, currentSortDirection, sortType);
                sortMenu.style.display = 'none';
            });
        });

        // Close sort menu when clicking outside
        document.addEventListener('click', function(e) {
            if (!e.target.closest('th.sortable') && !e.target.closest('#sortMenu')) {
                sortMenu.style.display = 'none';
            }
        });
    </script>
</body>
</html>`

		t, err := template.New("metrics").Parse(tmpl)
		if err != nil {
			http.Error(w, "Template parsing error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		err = t.Execute(w, metrics)
		if err != nil {
			http.Error(w, "Template execution error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	http.HandleFunc("/pod/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 4 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		namespace := parts[2]
		podName := parts[3]

		metrics, err := getPodMetricsHistory(db, namespace, podName)
		if err != nil {
			http.Error(w, "Failed to retrieve pod metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Prepare data for charts
		timestamps := make([]string, len(metrics))
		cpuValues := make([]float64, len(metrics))
		memoryValues := make([]float64, len(metrics))

		for i, m := range metrics {
			timestamps[i] = m.Timestamp
			cpuValues[i] = m.CPUCores
			memoryValues[i] = m.MemoryMB
		}

		tmpl := `
<!DOCTYPE html>
<html>
<head>
    <title>Pod Metrics - {{.PodName}}</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/hammerjs@2.0.8"></script>
    <script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-zoom@2.0.1"></script>
    <style>
        :root {
            --bg-color: #1a1a1a;
            --text-color: #e0e0e0;
            --border-color: #333;
            --hover-color: #2d2d2d;
            --link-color: #66b3ff;
            --button-bg: #333;
            --button-hover: #444;
        }

        body { 
            font-family: Arial, sans-serif; 
            margin: 20px;
            background-color: var(--bg-color);
            color: var(--text-color);
        }
        h1 { color: var(--text-color); }
        .chart-container { 
            width: 800px; 
            height: 400px; 
            margin: 20px 0; 
            position: relative;
            background-color: var(--bg-color);
            border: 1px solid var(--border-color);
            border-radius: 4px;
            padding: 10px;
        }
        .back-link {
            display: inline-block;
            margin-bottom: 20px;
            color: var(--link-color);
            text-decoration: none;
        }
        .back-link:hover {
            text-decoration: underline;
        }
        .chart-controls {
            margin: 10px 0;
        }
        .chart-controls button {
            padding: 8px 16px;
            margin-right: 10px;
            border: 1px solid var(--border-color);
            border-radius: 4px;
            background: var(--button-bg);
            color: var(--text-color);
            cursor: pointer;
        }
        .chart-controls button:hover {
            background: var(--button-hover);
        }
    </style>
</head>
<body>
    <a href="/metrics" class="back-link">← Back to all pods</a>
    <h1>Pod Metrics - {{.PodName}} ({{.Namespace}})</h1>
    
    <div class="chart-container">
        <div class="chart-controls">
            <button onclick="resetZoomCPU()">Reset Zoom (CPU)</button>
        </div>
        <canvas id="cpuChart"></canvas>
    </div>
    
    <div class="chart-container">
        <div class="chart-controls">
            <button onclick="resetZoomMemory()">Reset Zoom (Memory)</button>
        </div>
        <canvas id="memoryChart"></canvas>
    </div>

    <script>
        const timestamps = {{.Timestamps}};
        const cpuData = {{.CPUValues}};
        const memoryData = {{.MemoryValues}};

        let cpuChart, memoryChart;

        // CPU Chart
        cpuChart = new Chart(document.getElementById('cpuChart'), {
            type: 'line',
            data: {
                labels: timestamps,
                datasets: [{
                    label: 'CPU Usage (cores)',
                    data: cpuData,
                    borderColor: 'rgb(75, 192, 192)',
                    tension: 0.1
                }]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                layout: {
                    padding: {
                        bottom: 25
                    }
                },
                scales: {
                    y: {
                        beginAtZero: true,
                        title: {
                            display: true,
                            text: 'CPU Cores',
                            color: '#e0e0e0'
                        },
                        grid: {
                            color: '#333'
                        },
                        ticks: {
                            color: '#e0e0e0'
                        }
                    },
                    x: {
                        grid: {
                            color: '#333'
                        },
                        ticks: {
                            color: '#e0e0e0',
                            maxRotation: 45,
                            minRotation: 45,
                            autoSkip: true,
                            maxTicksLimit: 12,
                            padding: 10
                        }
                    }
                },
                plugins: {
                    zoom: {
                        pan: {
                            enabled: true,
                            mode: 'x'
                        },
                        zoom: {
                            wheel: {
                                enabled: true
                            },
                            pinch: {
                                enabled: true
                            },
                            mode: 'x',
                            drag: {
                                enabled: true,
                                backgroundColor: 'rgba(75,192,192,0.1)'
                            }
                        }
                    },
                    legend: {
                        labels: {
                            color: '#e0e0e0'
                        }
                    }
                }
            }
        });

        // Memory Chart
        memoryChart = new Chart(document.getElementById('memoryChart'), {
            type: 'line',
            data: {
                labels: timestamps,
                datasets: [{
                    label: 'Memory Usage (MB)',
                    data: memoryData,
                    borderColor: 'rgb(153, 102, 255)',
                    tension: 0.1
                }]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                layout: {
                    padding: {
                        bottom: 25
                    }
                },
                scales: {
                    y: {
                        beginAtZero: true,
                        title: {
                            display: true,
                            text: 'Memory (MB)',
                            color: '#e0e0e0'
                        },
                        grid: {
                            color: '#333'
                        },
                        ticks: {
                            color: '#e0e0e0'
                        }
                    },
                    x: {
                        grid: {
                            color: '#333'
                        },
                        ticks: {
                            color: '#e0e0e0',
                            maxRotation: 45,
                            minRotation: 45,
                            autoSkip: true,
                            maxTicksLimit: 12,
                            padding: 10
                        }
                    }
                },
                plugins: {
                    zoom: {
                        pan: {
                            enabled: true,
                            mode: 'x'
                        },
                        zoom: {
                            wheel: {
                                enabled: true
                            },
                            pinch: {
                                enabled: true
                            },
                            mode: 'x',
                            drag: {
                                enabled: true,
                                backgroundColor: 'rgba(153,102,255,0.1)'
                            }
                        }
                    },
                    legend: {
                        labels: {
                            color: '#e0e0e0'
                        }
                    }
                }
            }
        });

        function resetZoomCPU() {
            cpuChart.resetZoom();
        }

        function resetZoomMemory() {
            memoryChart.resetZoom();
        }
    </script>
</body>
</html>`

		data := struct {
			PodName      string
			Namespace    string
			Timestamps   []string
			CPUValues    []float64
			MemoryValues []float64
		}{
			PodName:      podName,
			Namespace:    namespace,
			Timestamps:   timestamps,
			CPUValues:    cpuValues,
			MemoryValues: memoryValues,
		}

		t, err := template.New("pod").Parse(tmpl)
		if err != nil {
			http.Error(w, "Template parsing error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		err = t.Execute(w, data)
		if err != nil {
			http.Error(w, "Template execution error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	address := "192.168.13.1:" + port
	fmt.Printf("Starting web server on %s...\n", address)
	return http.ListenAndServe(address, nil)
}

// getPodMetrics retrieves pod metrics from the database
func getPodMetrics(db *sql.DB) ([]PodMetric, error) {
	query := `
	WITH LatestMetrics AS (
		SELECT 
			m1.pod_name,
			m1.namespace,
			m1.timestamp,
			m1.cpu_cores,
			m1.memory_bytes,
			(SELECT MAX(cpu_cores) FROM pod_metrics m2 
			 WHERE m2.pod_name = m1.pod_name AND m2.namespace = m1.namespace) as max_cpu,
			(SELECT MAX(memory_bytes) FROM pod_metrics m2 
			 WHERE m2.pod_name = m1.pod_name AND m2.namespace = m1.namespace) as max_memory
		FROM pod_metrics m1
		WHERE (m1.pod_name, m1.namespace, m1.timestamp) IN (
			SELECT pod_name, namespace, MAX(timestamp)
			FROM pod_metrics
			GROUP BY pod_name, namespace
		)
	)
	SELECT * FROM LatestMetrics
	ORDER BY timestamp DESC
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metrics []PodMetric
	for rows.Next() {
		var timestamp time.Time
		var podName, namespace string
		var cpuCores, maxCPU float64
		var memoryBytes, maxMemoryBytes int64

		if err := rows.Scan(&podName, &namespace, &timestamp, &cpuCores, &memoryBytes, &maxCPU, &maxMemoryBytes); err != nil {
			return nil, err
		}

		metrics = append(metrics, PodMetric{
			Timestamp:   timestamp.Format("2006-01-02 15:04:05"),
			PodName:     podName,
			Namespace:   namespace,
			CPUCores:    cpuCores,
			MemoryMB:    float64(memoryBytes) / 1024 / 1024,
			MaxCPUCores: maxCPU,
			MaxMemoryMB: float64(maxMemoryBytes) / 1024 / 1024,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return metrics, nil
}

// getPodMetricsHistory retrieves the last 24 hours of metrics for a specific pod
func getPodMetricsHistory(db *sql.DB, namespace, podName string) ([]PodMetric, error) {
	query := `
	SELECT timestamp, pod_name, namespace, cpu_cores, memory_bytes
	FROM pod_metrics
	WHERE namespace = ? AND pod_name = ? 
	AND timestamp >= datetime('now', '-24 hours')
	ORDER BY timestamp ASC
	`

	rows, err := db.Query(query, namespace, podName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metrics []PodMetric
	for rows.Next() {
		var timestamp time.Time
		var podName, namespace string
		var cpuCores float64
		var memoryBytes int64

		if err := rows.Scan(&timestamp, &podName, &namespace, &cpuCores, &memoryBytes); err != nil {
			return nil, err
		}

		metrics = append(metrics, PodMetric{
			Timestamp: timestamp.Format("15:04:05"),
			PodName:   podName,
			Namespace: namespace,
			CPUCores:  cpuCores,
			MemoryMB:  float64(memoryBytes) / 1024 / 1024,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return metrics, nil
}
