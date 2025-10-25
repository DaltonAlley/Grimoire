document.addEventListener('DOMContentLoaded', () => {
    const submit2Button = document.getElementById('submit2') as HTMLButtonElement;
    const textarea = document.querySelector('textarea[name="Decklist"]') as HTMLTextAreaElement;

    if (submit2Button && textarea) {
        submit2Button.addEventListener('click', async (event) => {
            event.preventDefault();

            const decklist = textarea.value.trim();
            if (!decklist) {
                alert('Please enter a decklist');
                return;
            }

            try {
                const formData = new FormData();
                formData.append('Decklist', decklist);

                const apiResponse = await fetch('http://localhost:8081/api/submit', {
                    method: 'POST',
                    body: formData
                });

                const result = await apiResponse.json();
                
                if (apiResponse.ok) {
                    textarea.value = '';
                    const jobId = result.job_id;

                    if (jobId) {
                        htmx.ajax('GET', `/submit/${jobId}`, {
                            target: '#jobContainer',
                            swap: 'afterbegin',
                        }).then(() => {
                            console.log(`Job partial for ID ${jobId} loaded successfully.`);
                        }).catch(err => {
                            console.error(`Error loading job partial for ID ${jobId}:`, err);
                            alert(`Job submitted, but failed to display details for ID ${jobId}. Please check the console.`);
                        });
                    } else {
                        console.error('Job ID not found in the API response. Cannot load job partial.');
                        alert('Job submitted, but a unique ID was not returned. Cannot display job details.');
                    }
                } else {
                    alert(`Error: ${result.error || 'Unknown error occurred'}`);
                }
            } catch (error) {
                console.error('Error submitting form:', error);
                alert('Error submitting form. Please try again.');
            }
        });
    } else {
        console.error('submit2 button or textarea not found');
    }
});